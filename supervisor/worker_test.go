package supervisor_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/pkg/errors"
	que "github.com/qor5/go-que"
	"github.com/stretchr/testify/require"

	"github.com/qor5/go-que/supervisor"
)

// recordingOnStop captures the OnStop callback invocations.
type recordingOnStop struct {
	mu    sync.Mutex
	calls []error
}

func (r *recordingOnStop) fn(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, err)
}

func (r *recordingOnStop) Calls() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.calls...)
}

// failingMutex makes every Lock fail, so Worker.Run() returns an error.
type failingMutex struct{}

func (failingMutex) Lock(_ context.Context, _ string, _ int) ([]que.Job, error) {
	return nil, errors.New("simulated lock failure")
}
func (failingMutex) Unlock(_ context.Context, _ []int64) error { return nil }

// idleMutex never returns jobs and never errors, so the worker idles until Stop.
type idleMutex struct{}

func (idleMutex) Lock(_ context.Context, _ string, _ int) ([]que.Job, error) { return nil, nil }
func (idleMutex) Unlock(_ context.Context, _ []int64) error                  { return nil }

func noopPerform(_ context.Context, _ que.Job) error { return nil }

// TestStartWorker_OnStopReportsRealErrorOnAbnormalTermination verifies that
// when the worker gives up, OnStop fires once carrying the real Run() error
// (not the generic ErrReconnectBackOffStopped that Err() reports).
func TestStartWorker_OnStopReportsRealErrorOnAbnormalTermination(t *testing.T) {
	rec := &recordingOnStop{}
	ctrl, err := supervisor.StartWorker(context.Background(), que.WorkerOptions{
		Queue:                     "test-queue",
		Mutex:                     failingMutex{},
		Perform:                   noopPerform,
		MaxLockPerSecond:          100,
		MaxPerformPerSecond:       100,
		MaxConcurrentPerformCount: 1,
	},
		supervisor.WithReconnectBackOff(&backoff.StopBackOff{}), // give up on first failure
		supervisor.WithOnStop(rec.fn),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctrl.Stop(ctx)
	})

	select {
	case <-ctrl.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("worker should give up and close Done()")
	}
	require.Error(t, ctrl.Err())

	calls := rec.Calls()
	require.Len(t, calls, 1, "OnStop must fire exactly once on terminal failure")
	require.Error(t, calls[0])
	require.ErrorContains(t, calls[0], "simulated lock failure",
		"OnStop must carry the real Run() error, not ErrReconnectBackOffStopped")
}

// TestStartWorker_OnStopNilOnCleanStop verifies a graceful Stop() invokes
// OnStop once with a nil error.
func TestStartWorker_OnStopNilOnCleanStop(t *testing.T) {
	rec := &recordingOnStop{}
	ctrl, err := supervisor.StartWorker(context.Background(), que.WorkerOptions{
		Queue:                     "test-queue",
		Mutex:                     idleMutex{},
		Perform:                   noopPerform,
		MaxLockPerSecond:          100,
		MaxPerformPerSecond:       100,
		MaxConcurrentPerformCount: 1,
	}, supervisor.WithOnStop(rec.fn))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ctrl.Stop(ctx))
	<-ctrl.Done()
	require.NoError(t, ctrl.Err())

	calls := rec.Calls()
	require.Len(t, calls, 1, "OnStop must fire once on clean stop")
	require.NoError(t, calls[0], "clean stop must report a nil error")
}

// spyBackOff waits 1ms between attempts and counts NextBackOff and Reset calls.
type spyBackOff struct {
	mu          sync.Mutex
	next, reset int
}

func (b *spyBackOff) NextBackOff() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	return time.Millisecond
}

func (b *spyBackOff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reset++
}

func (b *spyBackOff) counts() (next, reset int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.next, b.reset
}

func startWithSpy(t *testing.T, mutex que.Mutex) *spyBackOff {
	t.Helper()
	spy := &spyBackOff{}
	ctrl, err := supervisor.StartWorker(context.Background(), que.WorkerOptions{
		Queue:                     "test-queue",
		Mutex:                     mutex,
		Perform:                   noopPerform,
		MaxLockPerSecond:          1000,
		MaxPerformPerSecond:       100,
		MaxConcurrentPerformCount: 1,
	}, supervisor.WithReconnectBackOff(spy))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctrl.Stop(ctx)
	})
	return spy
}

// TestStartWorker_BackOffGrowsWhileWorkersKeepFailing verifies that the
// reconnect backoff is not reset just because a worker was recreated: while
// the database stays unreachable every recreated worker fails at once, and the
// waits between attempts must keep growing.
func TestStartWorker_BackOffGrowsWhileWorkersKeepFailing(t *testing.T) {
	spy := startWithSpy(t, failingMutex{})
	require.Eventually(t, func() bool { next, _ := spy.counts(); return next >= 5 }, 5*time.Second, time.Millisecond)
	_, reset := spy.counts()
	require.Zero(t, reset, "the backoff must not start over while workers keep failing")
}

// flakyMutex locks fine, then fails the next Lock, and so on: each worker locks
// at least once before it fails, as when connections are cut while the
// database stays up.
type flakyMutex struct {
	mu    sync.Mutex
	calls int
}

func (m *flakyMutex) Lock(_ context.Context, _ string, _ int) ([]que.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls++; m.calls%2 == 0 {
		return nil, errors.New("simulated lock failure")
	}
	return nil, nil
}
func (*flakyMutex) Unlock(_ context.Context, _ []int64) error { return nil }

// TestStartWorker_BackOffStartsOverAfterAWorkerLocked verifies that a worker
// that locked successfully before it failed starts the reconnect backoff over:
// the database was reachable, so the next attempt need not wait long.
func TestStartWorker_BackOffStartsOverAfterAWorkerLocked(t *testing.T) {
	spy := startWithSpy(t, &flakyMutex{})
	require.Eventually(t, func() bool { next, _ := spy.counts(); return next >= 3 }, 5*time.Second, time.Millisecond)
	next, reset := spy.counts()
	require.GreaterOrEqual(t, reset, next, "each failure after a successful Lock starts the backoff over")
}

// lockOnceMutex locks fine once, then fails every Lock: the database was
// reachable for the first worker only.
type lockOnceMutex struct {
	mu     sync.Mutex
	locked bool
}

func (m *lockOnceMutex) Lock(_ context.Context, _ string, _ int) ([]que.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locked {
		return nil, errors.New("simulated lock failure")
	}
	m.locked = true
	return nil, nil
}
func (*lockOnceMutex) Unlock(_ context.Context, _ []int64) error { return nil }

// TestStartWorker_OnlyAWorkerThatLockedStartsTheBackOffOver verifies that the
// successful Lock is counted per worker: after the first worker locked and
// failed, recreated workers that never lock keep the backoff growing.
func TestStartWorker_OnlyAWorkerThatLockedStartsTheBackOffOver(t *testing.T) {
	spy := startWithSpy(t, &lockOnceMutex{})
	require.Eventually(t, func() bool { next, _ := spy.counts(); return next >= 5 }, 5*time.Second, time.Millisecond)
	_, reset := spy.counts()
	require.Equal(t, 1, reset, "only the worker that locked starts the backoff over")
}
