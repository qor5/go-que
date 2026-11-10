package que_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
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

// A job that a Lock running while Stop is called hands out is still unlocked:
// the worker does not perform it, and leaves it to other workers.
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
	time.Sleep(700 * time.Millisecond) // Stop has looked at least once while Lock runs
	close(mu.release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if err := <-ran; !errors.Is(err, que.ErrWorkerStoped) {
		t.Fatalf("want Run to return %v but get %v", que.ErrWorkerStoped, err)
	}
	mu.mu.Lock()
	defer mu.mu.Unlock()
	if len(mu.unlocked) != 1 || mu.unlocked[0] != 7 {
		t.Fatalf("want job 7 unlocked once Stop returns but get %v", mu.unlocked)
	}
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
