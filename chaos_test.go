package que_test

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qor5/go-que"
	"github.com/qor5/go-que/pg"
)

// TestChaos shares mutexes between manual Lock/Unlock loops and workers that
// are stopped with short timeouts and rebuilt on the same mutex, while callers
// use short contexts and backends are terminated at random. With
// QUE_CHAOS_PAUSE_CONTAINER set, it also freezes that Docker container now
// and then (a database that stops answering).
//
// It checks that a mutex never hands out a job its caller still has, that
// every job gets done, and that afterwards every mutex still works and no
// connection or goroutine is left behind. Run it with QUE_CHAOS=1; it takes
// tens of seconds.
func TestChaos(t *testing.T) {
	if os.Getenv("QUE_CHAOS") == "" {
		t.Skip("set QUE_CHAOS=1 to run")
	}
	const nJobs, nMutex, loopsPerMutex = 8000, 3, 4
	db, err := sql.Open(os.Getenv("QUE_DB_DRIVER"), os.Getenv("QUE_DB_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(12)
	defer func() { _ = db.Close() }()
	admin, err := sql.Open(os.Getenv("QUE_DB_DRIVER"), os.Getenv("QUE_DB_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	q, err := pg.New(db)
	if err != nil {
		t.Fatal(err)
	}
	qs := randQueue()
	for range nJobs {
		if _, err := q.Enqueue(context.Background(), nil, que.Plan{Queue: qs, Args: que.Args(), RunAt: due()}); err != nil {
			t.Fatal(err)
		}
	}
	goroutines := runtime.NumGoroutine()

	var doneIDs sync.Map
	var done, dup, kills atomic.Int64
	var errKinds sync.Map
	stop := make(chan struct{})
	var chaos sync.WaitGroup
	chaos.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(10+rand.IntN(60)) * time.Millisecond):
			}
			r, err := admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid()
				AND backend_type = 'client backend' ORDER BY random() LIMIT 1`)
			if err == nil {
				n, _ := r.RowsAffected()
				kills.Add(n)
			}
		}
	})
	if container := os.Getenv("QUE_CHAOS_PAUSE_CONTAINER"); container != "" {
		chaos.Go(func() {
			defer func() { _ = exec.Command("docker", "unpause", container).Run() }()
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Duration(500+rand.IntN(1500)) * time.Millisecond):
				}
				_ = exec.Command("docker", "pause", container).Run()
				time.Sleep(time.Duration(100+rand.IntN(700)) * time.Millisecond)
				_ = exec.Command("docker", "unpause", container).Run()
			}
		})
	}

	shortCtx := func() (context.Context, context.CancelFunc) {
		if rand.IntN(3) == 0 {
			return context.WithTimeout(context.Background(), time.Duration(1+rand.IntN(30))*time.Millisecond)
		}
		return context.WithCancel(context.Background())
	}
	deadline := time.Now().Add(3 * time.Minute)
	running := func() bool { return done.Load() < nJobs && time.Now().Before(deadline) }
	finish := func(jb que.Job) {
		for jb.Done(context.Background()) != nil {
		}
		if _, loaded := doneIDs.LoadOrStore(jb.ID(), true); !loaded {
			done.Add(1)
		}
	}

	var wg sync.WaitGroup
	mutexes := make([]que.Mutex, nMutex)
	for i := range mutexes {
		mu := q.Mutex()
		mutexes[i] = mu
		var heldMu sync.Mutex
		held := map[int64]bool{} // handed out by mu and not yet unlocked
		take := func(id int64) {
			heldMu.Lock()
			defer heldMu.Unlock()
			if held[id] {
				dup.Add(1)
			}
			held[id] = true
		}
		release := func(id int64) {
			heldMu.Lock()
			defer heldMu.Unlock()
			delete(held, id)
		}
		for range loopsPerMutex {
			wg.Go(func() {
				for running() {
					ctx, cancel := shortCtx()
					jobs, err := mu.Lock(ctx, qs, 1+rand.IntN(3))
					cancel()
					if err != nil {
						errKinds.Store("lock: "+errKind(err), true)
						time.Sleep(time.Millisecond)
						continue
					}
					ids := make([]int64, 0, len(jobs))
					for _, jb := range jobs {
						take(jb.ID())
						time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
						finish(jb)
						release(jb.ID())
						ids = append(ids, jb.ID())
					}
					if len(ids) > 0 {
						ctx, cancel := shortCtx()
						if err := mu.Unlock(ctx, ids); err != nil {
							errKinds.Store("unlock: "+errKind(err), true)
						}
						cancel()
					}
				}
			})
		}
		// a worker on the same mutex, stopped with a short timeout and rebuilt
		wg.Go(func() {
			for running() {
				w, err := que.NewWorker(que.WorkerOptions{
					Queue:                     qs,
					Mutex:                     mu,
					MaxLockPerSecond:          50,
					MaxPerformPerSecond:       1000,
					MaxConcurrentPerformCount: 3,
					Perform: func(ctx context.Context, jb que.Job) error {
						take(jb.ID())
						// release before returning: the worker unlocks the job
						// once Perform is over, and the mutex may hand it out
						// again right after
						defer release(jb.ID())
						time.Sleep(time.Duration(rand.IntN(20)) * time.Millisecond)
						finish(jb)
						return nil
					},
				})
				if err != nil {
					t.Error(err)
					return
				}
				ran := make(chan struct{})
				go func() {
					defer close(ran)
					if err := w.Run(); err != nil && !errors.Is(err, que.ErrWorkerStoped) {
						errKinds.Store("worker: "+errKind(err), true)
					}
				}()
				select {
				case <-ran:
				case <-time.After(time.Duration(50+rand.IntN(500)) * time.Millisecond):
					ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rand.IntN(10))*time.Millisecond)
					_ = w.Stop(ctx)
					cancel()
					<-ran
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	chaos.Wait()

	var left int
	if err := admin.QueryRow(`SELECT count(*) FROM goque_jobs WHERE queue = $1 AND done_at IS NULL`, qs).Scan(&left); err != nil {
		t.Fatal(err)
	}
	t.Logf("backends terminated: %d, jobs done: %d, left: %d", kills.Load(), done.Load(), left)
	errKinds.Range(func(k, _ any) bool { t.Logf("seen %v", k); return true })
	if n := dup.Load(); n != 0 {
		t.Errorf("a mutex handed out a job its caller still had, %d times", n)
	}
	if left != 0 {
		t.Errorf("%d jobs never done", left)
	}
	// the pool's idle connections may have been terminated last: drop them
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(2)
	for i, mu := range mutexes {
		if _, err := q.Enqueue(context.Background(), nil, que.Plan{Queue: qs, Args: que.Args(), RunAt: due()}); err != nil {
			t.Fatal(err)
		}
		// a mutex whose connection was terminated while idle fails its next
		// call, then connects again
		jobs, err := mu.Lock(context.Background(), qs, 1)
		if err != nil {
			jobs, err = mu.Lock(context.Background(), qs, 1)
		}
		if err != nil || len(jobs) != 1 {
			t.Errorf("mutex %d unusable afterwards: %v, %d jobs", i, err, len(jobs))
			continue
		}
		finish(jobs[0])
		if err := mu.Unlock(context.Background(), []int64{jobs[0].ID()}); err != nil {
			t.Errorf("mutex %d cannot unlock afterwards: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := db.Stats().InUse; n > nMutex {
		t.Errorf("%d connections in use, more than the %d mutexes hold", n, nMutex)
	}
	if n := runtime.NumGoroutine(); n > goroutines+2*nMutex+5 {
		t.Errorf("%d goroutines left, %d before", n, goroutines)
	}
}

func errKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "context"
	case errors.Is(err, que.ErrUnlockedJobs):
		return "ErrUnlockedJobs"
	}
	s := err.Error()
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
