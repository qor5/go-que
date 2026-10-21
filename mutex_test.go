package que_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/qor5/go-que"
	"github.com/qor5/go-que/pg"
)

// faults makes the next statement containing a given text fail once, or
// runs a statement on the same connection before it.
type faults struct {
	mu        sync.Mutex
	next      string
	before    string
	run       string
	breakRows string
	breakErr  error
	down      bool
	seen      map[string]int // statements run, by the texts registered with count
	hangOn    string
	hanging   chan struct{} // closed once a statement hangs
	unhang    chan struct{} // closing it lets the statement go on
}

// hang makes the next statement containing text block, whatever its context,
// until the returned func is called: a database that stops answering, with a
// driver that does not watch the context there (lib/pq while connecting).
func (f *faults) hang(text string) (hanging <-chan struct{}, unhang func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangOn, f.hanging, f.unhang = text, make(chan struct{}), make(chan struct{})
	return f.hanging, sync.OnceFunc(func() { close(f.unhang) })
}

// count starts counting the statements that contain text.
func (f *faults) count(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	f.seen[text] = 0
}

func (f *faults) counted(text string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[text]
}

func (f *faults) note(query string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for text := range f.seen {
		if strings.Contains(query, text) {
			f.seen[text]++
		}
	}
}

func (f *faults) hangs(query string) {
	f.mu.Lock()
	if f.hangOn == "" || !strings.Contains(query, f.hangOn) {
		f.mu.Unlock()
		return
	}
	f.hangOn = ""
	hanging, unhang := f.hanging, f.unhang
	f.mu.Unlock()
	close(hanging)
	<-unhang
}

// unreachable makes new connections fail while it is set.
func (f *faults) unreachable(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *faults) isDown() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.down
}

// breakRowsOf makes the rows of the next statement containing text fail after
// the first row.
func (f *faults) breakRowsOf(text string) { f.breakRowsWith(text, errInjected) }

// breakRowsWith is breakRowsOf failing with err.
func (f *faults) breakRowsWith(text string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breakRows, f.breakErr = text, err
}

func (f *faults) breaksRows(query string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.breakRows != "" && strings.Contains(query, f.breakRows) {
		f.breakRows = ""
		return f.breakErr
	}
	return nil
}

type brokenRows struct {
	driver.Rows
	err error
	n   int
}

func (r *brokenRows) Next(dest []driver.Value) error {
	if r.n++; r.n > 1 {
		return r.err
	}
	return r.Rows.Next(dest)
}

func (f *faults) runBefore(text, stmt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before, f.run = text, stmt
}

func (f *faults) prelude(query string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.before != "" && strings.Contains(query, f.before) {
		f.before = ""
		return f.run
	}
	return ""
}

func (f *faults) failNext(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = text
}

func (f *faults) fails(query string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next != "" && strings.Contains(query, f.next) {
		f.next = ""
		return true
	}
	return false
}

var errInjected = errors.New("injected fault")

type faultyConnector struct {
	driver.Connector
	f *faults
}

func (c faultyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if c.f.isDown() {
		return nil, errInjected
	}
	cn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return faultyConn{cn, c.f}, nil
}

type faultyConn struct {
	driver.Conn
	f *faults
}

func (c faultyConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.f.note(query)
	c.f.hangs(query)
	if c.f.fails(query) {
		return nil, errInjected
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c faultyConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.f.hangs(query)
	if c.f.fails(query) {
		return nil, errInjected
	}
	if stmt := c.f.prelude(query); stmt != "" {
		if _, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, stmt, nil); err != nil {
			return nil, err
		}
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err == nil {
		if berr := c.f.breaksRows(query); berr != nil {
			rows = &brokenRows{Rows: rows, err: berr}
		}
	}
	return rows, err
}

func newFaultyQueue(t *testing.T) (que.Queue, *faults) {
	t.Helper()
	newQueue() // migrates the shared database
	connector, err := pq.NewConnector(os.Getenv("QUE_DB_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	f := &faults{}
	db := sql.OpenDB(faultyConnector{connector, f})
	t.Cleanup(func() { _ = db.Close() })
	q, err := pg.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return q, f
}

func enqueueNow(t *testing.T, q que.Queue, qs string) int64 {
	t.Helper()
	ids, err := q.Enqueue(context.Background(), nil, que.Plan{
		Queue: qs,
		Args:  que.Args(),
		RunAt: time.Now().Add(-time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

func lockIDs(t *testing.T, mu que.Mutex, qs string, count int) []int64 {
	t.Helper()
	jobs, err := mu.Lock(context.Background(), qs, count)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(jobs))
	for _, jb := range jobs {
		ids = append(ids, jb.ID())
	}
	return ids
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[int64]bool{}
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			return false
		}
	}
	return true
}

func TestMutexRecoversAfterItsConnectionDies(t *testing.T) {
	q := newQueue()
	mu := q.Mutex()
	qs := randQueue()
	a := enqueueNow(t, q, qs)
	if got := lockIDs(t, mu, qs, 1); !equalIDs(got, []int64{a}) {
		t.Fatalf("want %d locked but get %v", a, got)
	}

	// the connection holding a's lock dies; a is still being performed
	var killed bool
	err := _db.QueryRow(`SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND objid = $1`, a).Scan(&killed)
	if err != nil || !killed {
		t.Fatalf("terminate the mutex's backend: %v %v", killed, err)
	}
	if _, err := mu.Lock(context.Background(), qs, 1); err == nil {
		t.Fatal("want the call on the dead connection to fail")
	}

	b := enqueueNow(t, q, qs)
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{b}) {
		t.Fatalf("want only %d (a is still with its caller) but get %v", b, got)
	}
	if err := mu.Unlock(context.Background(), []int64{a, b}); err != nil {
		t.Fatalf("unlock a job whose lock went with the connection: %v", err)
	}
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{a, b}) {
		t.Fatalf("want %d and %d lockable again but get %v", a, b, got)
	}
}

func TestMutexRecoversAfterSetupFails(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	a := enqueueNow(t, q, qs)

	f.failNext("CREATE TYPE pg_temp.goque_remaining_result")
	if _, err := mu.Lock(context.Background(), qs, 1); !errors.Is(err, errInjected) {
		t.Fatalf("want the injected setup error but get %v", err)
	}
	if got := lockIDs(t, mu, qs, 1); !equalIDs(got, []int64{a}) {
		t.Fatalf("want %d but get %v", a, got)
	}
}

func TestMutexDropsJobsAFailedLockNeverReturned(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	a := enqueueNow(t, q, qs)

	f.failNext("WHERE done_at IS NULL\n  AND expired_at IS NULL\n  AND id IN") // verifies the jobs it just locked
	if _, err := mu.Lock(context.Background(), qs, 1); !errors.Is(err, errInjected) {
		t.Fatalf("want the injected verify error but get %v", err)
	}
	if got := lockIDs(t, mu, qs, 1); !equalIDs(got, []int64{a}) {
		t.Fatalf("want %d lockable again (its caller never got it) but get %v", a, got)
	}
}

func TestMutexDropsItsSessionWhenTheDatabaseSaysAJobIsNotLocked(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	a, b := enqueueNow(t, q, qs), enqueueNow(t, q, qs)
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{a, b}) {
		t.Fatalf("want %d and %d but get %v", a, b, got)
	}

	// the session loses its locks behind the mutex's back, as through a
	// pooler that hands each transaction a different server connection
	f.runBefore("pg_advisory_unlock(v.i)", "SELECT pg_advisory_unlock_all()")
	if err := mu.Unlock(context.Background(), []int64{a}); !errors.Is(err, que.ErrUnlockedJobs) {
		t.Fatalf("want %v but get %v", que.ErrUnlockedJobs, err)
	}
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{a}) {
		t.Fatalf("want only %d (b is still with its caller) but get %v", a, got)
	}
	if err := mu.Unlock(context.Background(), []int64{a, b}); err != nil {
		t.Fatalf("want the session dropped and b's unlock local but get %v", err)
	}
}

func TestMutexUnlocksALostJobWhileTheDatabaseIsUnreachable(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	a := enqueueNow(t, q, qs)
	if got := lockIDs(t, mu, qs, 1); !equalIDs(got, []int64{a}) {
		t.Fatalf("want %d but get %v", a, got)
	}

	f.failNext("WITH RECURSIVE jobs") // a's lock goes with the connection
	if _, err := mu.Lock(context.Background(), qs, 1); !errors.Is(err, errInjected) {
		t.Fatalf("want the injected error but get %v", err)
	}
	f.unreachable(true)
	if err := mu.Unlock(context.Background(), []int64{a}); err != nil {
		t.Fatalf("unlocking a lost job needs no connection, but get %v", err)
	}
	f.unreachable(false)
	if got := lockIDs(t, mu, qs, 1); !equalIDs(got, []int64{a}) {
		t.Fatalf("want %d lockable again but get %v", a, got)
	}
}

func TestAFailedLockReturnsNoJobs(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	a, b := enqueueNow(t, q, qs), enqueueNow(t, q, qs)

	// a is done by the time Lock verifies it, and unlocking a fails
	f.runBefore("WHERE done_at IS NULL\n  AND expired_at IS NULL\n  AND id IN", fmt.Sprintf("UPDATE goque_jobs SET done_at = now() WHERE id = %d", a))
	f.failNext("pg_advisory_unlock(v.i)")
	jobs, err := mu.Lock(context.Background(), qs, 2)
	if !errors.Is(err, errInjected) || len(jobs) != 0 {
		t.Fatalf("want no jobs and the injected error but get %d jobs, %v", len(jobs), err)
	}
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{b}) {
		t.Fatalf("want %d (never handed out) but get %v", b, got)
	}
}

func TestUnlockFailsWhenItsReplyBreaksOff(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	a, b := enqueueNow(t, q, qs), enqueueNow(t, q, qs)
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{a, b}) {
		t.Fatalf("want %d and %d but get %v", a, b, got)
	}

	f.breakRowsOf("pg_advisory_unlock(v.i)")
	if err := mu.Unlock(context.Background(), []int64{a, b}); !errors.Is(err, errInjected) {
		t.Fatalf("want the broken reply reported but get %v", err)
	}
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{a, b}) {
		t.Fatalf("want both lockable again by the same mutex (the session is dropped, they are forgotten) but get %v", got)
	}
	if err := mu.Unlock(context.Background(), []int64{a, b}); err != nil {
		t.Fatal(err)
	}
}

func TestUnlockingAnUnknownJobKeepsTheSessionAndUnlocksTheRest(t *testing.T) {
	q := newQueue()
	mu := q.Mutex()
	qs := randQueue()
	a, b := enqueueNow(t, q, qs), enqueueNow(t, q, qs)
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{a, b}) {
		t.Fatalf("want %d and %d but get %v", a, b, got)
	}

	err := mu.Unlock(context.Background(), []int64{a, math.MaxInt64})
	var errQue *que.ErrQueue
	if !errors.Is(err, que.ErrUnlockedJobs) || !errors.As(err, &errQue) {
		t.Fatalf("want %v but get %v", que.ErrUnlockedJobs, err)
	}
	other := q.Mutex()
	if got := lockIDs(t, other, qs, 2); !equalIDs(got, []int64{a}) {
		t.Fatalf("want only %d (unlocked; b is still locked by the kept session) but get %v", a, got)
	}
	if err := other.Unlock(context.Background(), []int64{a}); err != nil {
		t.Fatal(err)
	}
	if err := mu.Unlock(context.Background(), []int64{b}); err != nil {
		t.Fatal(err)
	}
}

func TestUnlockingAJobTwiceInOneCallKeepsTheSession(t *testing.T) {
	q := newQueue()
	mu := q.Mutex()
	qs := randQueue()
	a, b := enqueueNow(t, q, qs), enqueueNow(t, q, qs)
	if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{a, b}) {
		t.Fatalf("want %d and %d but get %v", a, b, got)
	}

	if err := mu.Unlock(context.Background(), []int64{a, a}); err != nil {
		t.Fatalf("want a unlocked once but get %v", err)
	}
	other := q.Mutex()
	if got := lockIDs(t, other, qs, 2); !equalIDs(got, []int64{a}) {
		t.Fatalf("want only %d (b is still locked by the kept session) but get %v", a, got)
	}
	if err := other.Unlock(context.Background(), []int64{a}); err != nil {
		t.Fatal(err)
	}
	if err := mu.Unlock(context.Background(), []int64{b}); err != nil {
		t.Fatal(err)
	}
}

func TestLockFailsWhenItsReplyBreaksOff(t *testing.T) {
	const locking, verifying = "WITH RECURSIVE jobs", "WHERE done_at IS NULL\n  AND expired_at IS NULL\n  AND id IN"
	for name, c := range map[string]struct {
		text string
		err  error
	}{
		"locking":   {locking, errInjected},
		"verifying": {verifying, errInjected},
		// database/sql leaves the connection open after driver.ErrBadConn
		// from Rows.Next when Rows.Close succeeds: the mutex must still end
		// the session, or its locks stay with it
		"locking, bad connection":   {locking, driver.ErrBadConn},
		"verifying, bad connection": {verifying, driver.ErrBadConn},
	} {
		t.Run(name, func(t *testing.T) {
			q, f := newFaultyQueue(t)
			mu := q.Mutex()
			qs := randQueue()
			a, b := enqueueNow(t, q, qs), enqueueNow(t, q, qs)

			f.breakRowsWith(c.text, c.err)
			jobs, err := mu.Lock(context.Background(), qs, 2)
			if !errors.Is(err, c.err) || len(jobs) != 0 {
				t.Fatalf("want no jobs and the broken reply reported but get %d jobs, %v", len(jobs), err)
			}
			if got := lockIDs(t, mu, qs, 2); !equalIDs(got, []int64{a, b}) {
				t.Fatalf("want %d and %d lockable again but get %v", a, b, got)
			}
			if err := mu.Unlock(context.Background(), []int64{a, b}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnlockingAnUnknownJobWithoutAConnectionDoesNotConnect(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	f.failNext("CREATE TYPE pg_temp.goque_remaining_result") // connecting would fail
	if err := mu.Unlock(context.Background(), []int64{math.MaxInt64}); !errors.Is(err, que.ErrUnlockedJobs) {
		t.Fatalf("want %v without connecting but get %v", que.ErrUnlockedJobs, err)
	}
}

func TestUnlockingALostJobDoesNotWaitForAHangingReconnect(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	a := enqueueNow(t, q, qs)
	if got := lockIDs(t, mu, qs, 1); !equalIDs(got, []int64{a}) {
		t.Fatalf("want %d but get %v", a, got)
	}
	f.failNext("WITH RECURSIVE jobs") // a's lock goes with the connection: a is lost
	if _, err := mu.Lock(context.Background(), qs, 1); !errors.Is(err, errInjected) {
		t.Fatalf("want the injected error but get %v", err)
	}

	hanging, unhang := f.hang("CREATE TYPE pg_temp.goque_remaining_result")
	defer unhang()
	locked := make(chan error, 1)
	go func() {
		_, err := mu.Lock(context.Background(), qs, 1) // reconnects, and hangs there
		locked <- err
	}()
	<-hanging

	unlocked := make(chan error, 1)
	go func() { unlocked <- mu.Unlock(context.Background(), []int64{a}) }()
	select {
	case err := <-unlocked:
		if err != nil {
			t.Fatalf("want a's unlock local but get %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unlocking a lost job waited for the hanging reconnect")
	}
	unhang()
	if err := <-locked; err != nil {
		t.Fatalf("want the Lock to go on once the database answers but get %v", err)
	}
}

func TestDroppingASessionDoesNotWaitForTheDatabase(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	enqueueNow(t, q, qs)

	_, unhang := f.hang("drop function pg_temp") // a statement on the session being dropped would hang
	defer unhang()
	f.failNext("WITH RECURSIVE jobs")
	failed := make(chan error, 1)
	go func() {
		_, err := mu.Lock(context.Background(), qs, 1)
		failed <- err
	}()
	select {
	case err := <-failed:
		if !errors.Is(err, errInjected) {
			t.Fatalf("want the injected error but get %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dropping the session ran a statement on it and waited")
	}
}

func TestConcurrentLocksConnectOnce(t *testing.T) {
	q, f := newFaultyQueue(t)
	mu := q.Mutex()
	qs := randQueue()
	const setup = "CREATE TYPE pg_temp.goque_remaining_result"
	f.count(setup)
	hanging, unhang := f.hang(setup)
	defer unhang()

	const n = 5
	errs := make(chan error, n)
	go func() { _, err := mu.Lock(context.Background(), qs, 1); errs <- err }()
	<-hanging // the first Lock is connecting
	for range n - 1 {
		go func() { _, err := mu.Lock(context.Background(), qs, 1); errs <- err }()
	}
	time.Sleep(100 * time.Millisecond) // let the others reach the mutex
	unhang()
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := f.counted(setup); got != 1 {
		t.Fatalf("want one connection set up for %d concurrent Locks but get %d", n, got)
	}
}

func TestAWorkerRebuiltOnTheSameMutexKeepsWorking(t *testing.T) {
	q := newQueue()
	mu := q.Mutex()
	qs := randQueue()
	a := enqueueNow(t, q, qs)

	started, release := make(chan struct{}), make(chan struct{})
	performed := make(chan int64, 2)
	newWorker := func() *que.Worker {
		w, err := que.NewWorker(que.WorkerOptions{
			Queue:            qs,
			Mutex:            mu,
			MaxLockPerSecond: 20,
			Perform: func(ctx context.Context, jb que.Job) error {
				if jb.ID() == a {
					close(started)
					<-release
				}
				performed <- jb.ID()
				return jb.Destroy(ctx)
			},
			MaxPerformPerSecond: 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}

	w1 := newWorker()
	ran := make(chan error, 1)
	go func() { ran <- w1.Run() }()
	<-started
	var killed bool
	err := _db.QueryRow(`SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND objid = $1`, a).Scan(&killed)
	if err != nil || !killed {
		t.Fatalf("terminate the mutex's backend: %v %v", killed, err)
	}
	close(release)
	select {
	case err := <-ran:
		if err == nil {
			t.Fatal("want the worker to stop on the dead connection")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker kept running on a dead connection")
	}
	if got := <-performed; got != a {
		t.Fatalf("want %d performed but get %d", a, got)
	}

	b := enqueueNow(t, q, qs)
	w2 := newWorker()
	go func() { _ = w2.Run() }()
	defer func() { _ = w2.Stop(context.Background()) }()
	select {
	case got := <-performed:
		if got != b {
			t.Fatalf("want %d performed but get %d", b, got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a worker rebuilt on the same mutex performed nothing")
	}
}
