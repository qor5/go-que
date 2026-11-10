package que_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qor5/go-que"
)

// A Stop that times out still leaves the job it waited for to other workers
// once the job is over: a job that asked for a retry gets retried elsewhere.
func TestAJobOutlivingAStopThatTimedOutIsRetriedElsewhere(t *testing.T) {
	q := newQueue()
	qs := randQueue()
	if _, err := q.Enqueue(context.Background(), nil, que.Plan{Queue: qs, Args: que.Args(), RunAt: due()}); err != nil {
		t.Fatal(err)
	}
	started, finished := make(chan struct{}), make(chan struct{})
	w, err := que.NewWorker(que.WorkerOptions{
		Queue:            qs,
		Mutex:            q.Mutex(),
		MaxLockPerSecond: 10,
		Perform: func(ctx context.Context, job que.Job) error {
			close(started)
			time.Sleep(2 * time.Second) // outlives the Stop below
			defer close(finished)
			return job.RetryAfter(ctx, 0, errors.New("failed, try again"))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run() }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := w.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want Stop to time out but get %v", err)
	}
	<-finished

	other := q.Mutex()
	var jobs []que.Job
	for range 30 { // the stopped worker unlocks the job once it is over
		if jobs, err = other.Lock(context.Background(), qs, 1); err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(jobs) != 1 {
		t.Fatal("the job to retry stays locked by the stopped worker: no other worker can retry it")
	}
	_ = other.Unlock(context.Background(), []int64{jobs[0].ID()})
}

// slowMutex hands out one job from a Lock that returns only when told to,
// and records what is unlocked.
type slowMutex struct {
	locking  chan struct{} // closed once Lock is called
	release  chan struct{} // closing it lets Lock return
	mu       sync.Mutex
	calls    int
	unlocked []int64
}

func (m *slowMutex) Lock(ctx context.Context, queue string, count int) ([]que.Job, error) {
	m.mu.Lock()
	m.calls++
	first := m.calls == 1
	m.mu.Unlock()
	if !first {
		return nil, nil
	}
	close(m.locking)
	<-m.release
	return []que.Job{fakeJob(7)}, nil
}

func (m *slowMutex) Unlock(ctx context.Context, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unlocked = append(m.unlocked, ids...)
	return nil
}

// waitUnlocked waits until id is unlocked.
func (m *slowMutex) waitUnlocked(t *testing.T, id int64) {
	t.Helper()
	for range 200 {
		m.mu.Lock()
		for _, u := range m.unlocked {
			if u == id {
				m.mu.Unlock()
				return
			}
		}
		m.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want job %d unlocked but get %v", id, m.unlocked)
}

type fakeJob int64

func (j fakeJob) ID() int64                                            { return int64(j) }
func (fakeJob) Plan() que.Plan                                         { return que.Plan{} }
func (fakeJob) RetryCount() int32                                      { return 0 }
func (fakeJob) LastErrMsg() *string                                    { return nil }
func (fakeJob) LastErrStack() *string                                  { return nil }
func (fakeJob) In(*sql.Tx)                                             {}
func (fakeJob) Done(context.Context) error                             { return nil }
func (fakeJob) Destroy(context.Context) error                          { return nil }
func (fakeJob) Expire(context.Context, error) error                    { return nil }
func (fakeJob) RetryAfter(context.Context, time.Duration, error) error { return nil }
func (fakeJob) RetryInPlan(context.Context, error) error               { return nil }

// A job handed out by a Lock that was running when Stop returned is unlocked
// once that Lock returns: the worker does not perform it, and leaves it to
// other workers.
func TestAJobLockedWhileStoppingIsUnlocked(t *testing.T) {
	mu := &slowMutex{locking: make(chan struct{}), release: make(chan struct{})}
	w, err := que.NewWorker(que.WorkerOptions{
		Queue:            "q",
		Mutex:            mu,
		MaxLockPerSecond: 100,
		Perform: func(ctx context.Context, job que.Job) error {
			t.Error("a stopped worker performed a job")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- w.Run() }()
	<-mu.locking
	stopped := make(chan error, 1)
	go func() { stopped <- w.Stop(context.Background()) }()
	if err := <-stopped; err != nil { // Stop does not wait for the running Lock
		t.Fatal(err)
	}
	close(mu.release)
	if err := <-ran; !errors.Is(err, que.ErrWorkerStoped) {
		t.Fatalf("want Run to return %v but get %v", que.ErrWorkerStoped, err)
	}
	mu.waitUnlocked(t, 7)
}

// Stop on a worker that never ran returns at once.
func TestStopBeforeRun(t *testing.T) {
	w, err := que.NewWorker(que.WorkerOptions{Queue: "q", Mutex: &slowMutex{}, Perform: func(context.Context, que.Job) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Run(); !errors.Is(err, que.ErrWorkerStoped) {
		t.Fatalf("want %v but get %v", que.ErrWorkerStoped, err)
	}
}

// idleMutex never has a job; locked is closed on its first Lock.
type idleMutex struct {
	once   sync.Once
	locked chan struct{}
}

func (m *idleMutex) Lock(context.Context, string, int) ([]que.Job, error) {
	m.once.Do(func() { close(m.locked) })
	return nil, nil
}

func (m *idleMutex) Unlock(context.Context, []int64) error { return nil }

// Stop wakes a worker waiting for its next Lock: with a low lock rate, Run
// would otherwise return only after the whole interval.
func TestStopDoesNotWaitForTheLockRate(t *testing.T) {
	mu := &idleMutex{locked: make(chan struct{})}
	w, err := que.NewWorker(que.WorkerOptions{
		Queue:            "q",
		Mutex:            mu,
		MaxLockPerSecond: 0.01, // one Lock per 100s
		Perform:          func(context.Context, que.Job) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- w.Run() }()
	<-mu.locked
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatalf("want an idle worker to stop at once but get %v", err)
	}
	select {
	case err := <-ran:
		if !errors.Is(err, que.ErrWorkerStoped) {
			t.Fatalf("want %v but get %v", que.ErrWorkerStoped, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run waits for the lock rate after Stop")
	}
}

// Run returns as soon as Stop ends the run, as it always did, even while a
// Perform goes on; the job is still unlocked once that Perform is over.
func TestRunReturnsOnStopWhileAPerformGoesOn(t *testing.T) {
	mu := &slowMutex{locking: make(chan struct{}), release: make(chan struct{})}
	close(mu.release)
	performing, finish := make(chan struct{}), make(chan struct{})
	w, err := que.NewWorker(que.WorkerOptions{
		Queue:            "q",
		Mutex:            mu,
		MaxLockPerSecond: 100,
		Perform: func(context.Context, que.Job) error {
			close(performing)
			<-finish // a Perform that does not end with Stop
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- w.Run() }()
	<-performing
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want Stop to time out but get %v", err)
	}
	select {
	case err := <-ran:
		if !errors.Is(err, que.ErrWorkerStoped) {
			t.Fatalf("want %v but get %v", que.ErrWorkerStoped, err)
		}
	case <-time.After(2 * time.Second):
		close(finish)
		t.Fatal("Run waits for a Perform after Stop")
	}
	close(finish)
	if err := w.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.waitUnlocked(t, 7)
}

// scriptMutex hands out jobs as lock says, and records what is unlocked.
type scriptMutex struct {
	mu       sync.Mutex
	calls    int
	lock     func(call int) ([]que.Job, error)
	unlocked []int64
}

func (m *scriptMutex) Lock(ctx context.Context, queue string, count int) ([]que.Job, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	return m.lock(call)
}

func (m *scriptMutex) Unlock(ctx context.Context, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unlocked = append(m.unlocked, ids...)
	return nil
}

// waitUnlocked waits until ids are all unlocked.
func (m *scriptMutex) waitUnlocked(t *testing.T, ids ...int64) {
	t.Helper()
	for range 200 {
		m.mu.Lock()
		got := map[int64]bool{}
		for _, id := range m.unlocked {
			got[id] = true
		}
		m.mu.Unlock()
		all := true
		for _, id := range ids {
			all = all && got[id]
		}
		if all {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %v unlocked but get %v", ids, m.unlocked)
}

// A run that Stop ended returns at once even if its last Lock fails, as it
// did before; the job being performed is still unlocked once it is over.
func TestRunReturnsAtOnceWhenAStoppedRunsLockFails(t *testing.T) {
	locking, stopping, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	mu := &scriptMutex{lock: func(call int) ([]que.Job, error) {
		switch call {
		case 1:
			return []que.Job{fakeJob(7)}, nil
		case 2:
			close(locking)
			<-stopping
			return nil, errors.New("connection broken")
		}
		return nil, nil
	}}
	performing := make(chan struct{})
	w, err := que.NewWorker(que.WorkerOptions{
		Queue:                     "q",
		Mutex:                     mu,
		MaxLockPerSecond:          100,
		MaxConcurrentPerformCount: 2, // room to Lock again while job 7 runs
		Perform: func(context.Context, que.Job) error {
			close(performing)
			<-finish
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- w.Run() }()
	<-performing
	<-locking // the next Lock is running
	stopped := make(chan error, 1)
	go func() { stopped <- w.Stop(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // Stop is waiting for the Perform
	close(stopping)
	select {
	case err := <-ran:
		if err == nil || err.Error() != "connection broken" {
			t.Fatalf("want Run to return the Lock's error but get %v", err)
		}
	case <-time.After(2 * time.Second):
		close(finish)
		t.Fatal("Run waits for a Perform after Stop")
	}
	close(finish)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	mu.waitUnlocked(t, 7)
}

// A job a worker took but has not started performing is skipped once Stop is
// called, instead of being performed after the perform rate allows it.
func TestAStoppedWorkerDoesNotPerformAJobWaitingForThePerformRate(t *testing.T) {
	mu := &scriptMutex{lock: func(call int) ([]que.Job, error) {
		if call == 1 {
			return []que.Job{fakeJob(1), fakeJob(2)}, nil
		}
		return nil, nil
	}}
	var performed atomic.Int32
	first, finish := make(chan struct{}), make(chan struct{})
	w, err := que.NewWorker(que.WorkerOptions{
		Queue:                     "q",
		Mutex:                     mu,
		MaxLockPerSecond:          100,
		MaxPerformPerSecond:       0.2, // the second job waits 5s
		MaxConcurrentPerformCount: 2,
		Perform: func(context.Context, que.Job) error {
			if performed.Add(1) == 1 {
				close(first)
				<-finish
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- w.Run() }()
	<-first
	time.Sleep(50 * time.Millisecond) // the other job waits for the rate
	close(finish)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatalf("want Stop not to wait for the perform rate but get %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Stop took %v", d)
	}
	<-ran
	if n := performed.Load(); n != 1 {
		t.Fatalf("want only the job already being performed performed but get %d", n)
	}
	mu.waitUnlocked(t, 1, 2)
}
