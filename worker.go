package que

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// NewWorker creates a worker instance by given opts.
// The Queue, Mutex and Perform of opts are required.
// Other options is set to a default value if they are invalid.
func NewWorker(opts WorkerOptions) (*Worker, error) {
	if opts.Queue == "" {
		return nil, errors.New("queue must not be empty string")
	}
	if opts.Mutex == nil {
		return nil, errors.New("mutex must not be nil")
	}
	if opts.Perform == nil {
		return nil, errors.New("perform must not be nil")
	}
	if opts.MaxLockPerSecond <= 0 {
		opts.MaxLockPerSecond = 1
	}
	if opts.MaxPerformPerSecond <= 0 {
		opts.MaxPerformPerSecond = 1
	}
	if opts.MaxConcurrentPerformCount <= 0 {
		opts.MaxConcurrentPerformCount = 1
	}
	if opts.MaxBufferJobsCount < 0 {
		opts.MaxBufferJobsCount = 0
	}
	performContext := opts.PerformContext
	if performContext == nil {
		performContext = context.Background()
	}
	var count = opts.MaxConcurrentPerformCount + opts.MaxBufferJobsCount
	stopping, stop := context.WithCancel(context.Background())

	return &Worker{
		queue:                     opts.Queue,
		mutex:                     opts.Mutex,
		performContext:            performContext,
		perform:                   opts.Perform,
		maxConcurrentPerformCount: opts.MaxConcurrentPerformCount,
		lockLimiter:               newRateLimiter(opts.MaxLockPerSecond),
		jobC:                      make(chan Job, count),
		performLimiter:            newRateLimiter(opts.MaxPerformPerSecond),
		processed:                 make([]int64, 0, count),
		stopping:                  stopping,
		stop:                      stop,
	}, nil
}

func newRateLimiter(perSecond float64) *rate.Limiter {
	return rate.NewLimiter(rate.Limit(perSecond), 1)
}

// WorkerOptions is used for new a worker.
type WorkerOptions struct {
	// A queue instance must only or not assign to a worker.
	// When work stoped, inner queue is closed.
	Queue string
	Mutex Mutex
	// MaxLockPerSecond is maximum frequency calls Lock() of Queue.
	// Lower number uses lower database cpu.
	MaxLockPerSecond float64
	// MaxBufferJobsCount is maximum of jobs in chan that can't find
	// a goroutine execute it.
	MaxBufferJobsCount int

	// Perform is user special logic function.
	// context.Context is PerformContext.
	//
	// One of Done, Destroy and Expire method of Job must be called
	// in Perform when execute successfully, if not, Perform will be executed forever.
	//
	// When Perform panic or returns an error, RetryIn will be auto executed according to PerformRetryPolicy.
	// When PerformRetryPolicy says stop retry, Expire will be executed.
	Perform        func(context.Context, Job) error
	PerformContext context.Context
	// MaxPerformPerSecond is maximum frequency of Perform execution.
	MaxPerformPerSecond float64
	// MaxConcurrentPerformCount is maximum goroutine of Perform execution.
	MaxConcurrentPerformCount int
}

// Worker locks jobs from Queue and executes Perform method according to given WorkerOptions.
//
// Run a worker:
//
//    mutex := q.Mutex()
//    w, err := que.NewWorker(que.WorkerOptions{
//        Queue: "order.mail.register",
//        Mutex: mutex,
//        Perform: func(ctx context.Context, job que.Job) error {
//            plan := job.Plan()
//            args := decode(plan.Args)
//            fmt.Println(args)
//            // one of Done, Destroy, Expire, RetryAfter and RetryInPlan must be called.
//            return job.Done(ctx)
//        },
//    })
//    if err := w.Run(); err != nil {
//        log.Println(err)
//    }
//
// Reasonable quit worker execution:
//
//    if err := w.Stop(context.Background()); err != nil {
//        log.Println(err)
//    }
type Worker struct {
	queue string
	mutex Mutex

	perform        func(context.Context, Job) error
	performContext context.Context

	maxConcurrentPerformCount int
	lockLimiter               *rate.Limiter

	jobC               chan Job
	performLimiter     *rate.Limiter
	performRetryPolicy RetryPolicy

	ongoing int32

	mux       sync.Mutex
	processed []int64

	stopped int32
	// stopping ends when the worker stops, cutting short the waits for the
	// lock and perform rates.
	stopping context.Context
	stop     context.CancelFunc
	workers  sync.WaitGroup
	// late is the run's own unlock once it has taken jobs (guarded by mux).
	// The first Stop that finds nothing left to unlock takes it up: it waits
	// for it and returns its error, as it would have unlocked those jobs
	// itself; any other Stop returns as before.
	late *lateUnlock
}

type lateUnlock struct {
	done     chan struct{} // closed once err or panicked is set
	err      error
	panicked any  // what the Unlock panicked with, if it did
	adopted  bool // a Stop has taken it up (guarded by Worker.mux)
}

// Run acutal starts goroutine to execute Perform method and locks jobs.
// Run blocked current caller's goroutine and returns error when get unsolved error.
// When Run() returned, any call to it get ErrWorkerStoped.
//
// Before an error returns, the jobs being performed finish (unless Stop was
// called first: then Run returns the error at once, as it returns
// ErrWorkerStoped). To go on, create a new worker; it may use the same mutex,
// which connects again on its next call. Back off between attempts: while the
// database is unreachable each one fails.
func (w *Worker) Run() error {
	if w.isStopped() {
		return ErrWorkerStoped
	}

	for i := 0; i < w.maxConcurrentPerformCount; i++ {
		w.workers.Add(1)
		go func() {
			defer w.workers.Done()
			w.work()
		}()
	}
	return w.lock()
}

func (w *Worker) lock() (err error) {
	defer func() {
		byStop := w.isStopped()
		// mark the worker stopped before closing jobC, so no buffered job is
		// performed after an error: the mutex has likely dropped its session,
		// and those jobs' locks with it
		atomic.StoreInt32(&w.stopped, 1)
		w.stop()
		close(w.jobC)
		if byStop {
			// Stop waits for the jobs being performed and unlocks them;
			// unlock once more after them, for what a Stop that timed out,
			// or that returned while a Lock still ran, left locked
			go w.unlockAfterPerform()
			return
		}
		// an error ended the run: as on master, Stop waits for the jobs
		// being performed and unlocks them, then Run returns
		_ = w.Stop(context.Background())
		w.unlockAfterPerform()
	}()
	var jobs []Job
	for {
		_ = w.lockLimiter.Wait(w.stopping) // ends early when the worker stops
		if w.isStopped() {
			return ErrWorkerStoped
		}
		ongoingCount := int(atomic.LoadInt32(&w.ongoing))
		lockCount := cap(w.jobC) - len(w.jobC) - ongoingCount
		if lockCount > 0 {
			jobs, err = w.mutex.Lock(context.Background(), w.queue, lockCount)
			if err != nil {
				return err
			}
			for _, job := range jobs {
				atomic.AddInt32(&w.ongoing, 1)
				w.jobC <- job
			}
		}
		err = w.unlockProcessed()
		if err != nil {
			return err
		}
	}
}

// unlockAfterPerform unlocks the jobs the run has processed once every job
// taken from jobC is over. Only the run locks jobs and jobC is closed, so no
// job can be locked after this. A Stop may take up its result (see late).
func (w *Worker) unlockAfterPerform() {
	w.workers.Wait()
	w.mux.Lock()
	processed := w.processed
	if len(processed) == 0 {
		w.mux.Unlock()
		return
	}
	w.processed = make([]int64, 0, cap(processed))
	late := &lateUnlock{done: make(chan struct{})}
	w.late = late
	w.mux.Unlock()
	defer close(late.done)
	defer func() {
		// this goroutine is the worker's, where a panic would end the
		// process: the Stop that takes this up panics with it instead, in
		// its caller's goroutine, as when it did the Unlock itself
		if e := recover(); e != nil {
			late.panicked = e
			log.Printf("que: unlock of jobs %v panicked: %v", processed, e)
		}
	}()
	late.err = w.mutex.Unlock(context.Background(), processed)
}

// unlockForStop unlocks the processed jobs for Stop and returns the result.
// When the run's own unlock has already taken them and no Stop has taken it
// up yet, this Stop waits for it and returns its result instead, as it
// would have done that Unlock itself.
func (w *Worker) unlockForStop() error {
	w.mux.Lock()
	processed := w.processed
	if len(processed) > 0 {
		w.processed = make([]int64, 0, cap(processed))
	}
	late := w.late
	adopt := len(processed) == 0 && late != nil && !late.adopted
	if adopt {
		late.adopted = true
	}
	w.mux.Unlock()
	if len(processed) > 0 {
		return w.mutex.Unlock(context.Background(), processed)
	}
	if adopt {
		<-late.done
		if late.panicked != nil {
			panic(late.panicked)
		}
		return late.err
	}
	return nil
}

func (w *Worker) work() {
	for {
		job, ok := <-w.jobC
		if !ok {
			return
		}
		if w.isStopped() {
			w.asProcessed(job.ID())
			continue
		}

		_ = w.performLimiter.Wait(w.stopping) // ends early when the worker stops
		if w.isStopped() {
			w.asProcessed(job.ID())
			continue
		}
		w.tryPerform(job)
	}
}

func (w *Worker) tryPerform(job Job) {
	defer w.asProcessed(job.ID())

	var err error
	defer func() {
		e := recover()
		if e != nil {
			err = fmt.Errorf("%v", e)
		}
		if err != nil {
			err2 := w.handleErr(job, err)
			if err2 != nil {
				log.Printf(
					"que: perform(job(%v)) with err %v but handle err with a new err %v",
					job.ID(), err, err2,
				)
			}
		}
	}()
	err = w.perform(w.performContext, job)
}

func (w *Worker) handleErr(job Job, cerr error) error {
	return job.RetryInPlan(context.Background(), cerr)
}

func (w *Worker) asProcessed(id int64) {
	w.mux.Lock()
	w.processed = append(w.processed, id)
	w.mux.Unlock()
	atomic.AddInt32(&w.ongoing, -1)
}

func (w *Worker) unlockProcessed() error {
	var processed []int64
	w.mux.Lock()
	if len(w.processed) > 0 {
		processed = w.processed
		w.processed = make([]int64, 0, cap(processed))
	}
	w.mux.Unlock()
	var err error
	if len(processed) > 0 {
		err = w.mutex.Unlock(context.Background(), processed)
	}
	return err
}

func (w *Worker) isStopped() bool {
	return atomic.LoadInt32(&w.stopped) == 1
}

// Stop stops worker execution.
// It blocks until ctx.Done() or all processing jobs done. It does not cancel
// PerformContext. Jobs taken but not yet being performed (waiting for the
// perform rate, or buffered) are not performed; they are unlocked for other
// workers. Jobs it leaves locked (ctx ended first, or a Lock was still
// running) are unlocked by the run once their Perform is over, which may call
// the mutex's Unlock after Stop and Run have returned; the next Stop that
// finds nothing else to unlock waits for that Unlock and returns its error.
// Like any failing Unlock it drops the mutex's session, with the locks of
// anyone else using that mutex (it fails only when the connection is
// already broken, and those locks with it).
func (w *Worker) Stop(ctx context.Context) error {
	atomic.StoreInt32(&w.stopped, 1)
	if w.stop != nil { // nil on a Worker NewWorker did not make
		w.stop()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
wait:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if atomic.LoadInt32(&w.ongoing) == 0 {
				break wait
			}
		}
	}

	return w.unlockForStop()
}
