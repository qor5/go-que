package pg

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/qor5/go-que"
)

func TestMain(m *testing.M) {
	maxIdle = 200 * time.Millisecond // before any idle closer reads it
	os.Exit(m.Run())
}

// TestAnIdleCloserOfADroppedConnectionLeavesTheMutexAlone: a connection's idle
// closer can be waiting for the mutex while a call that fails drops that
// connection; once it gets the mutex it must not touch the mutex's state.
func TestAnIdleCloserOfADroppedConnectionLeavesTheMutexAlone(t *testing.T) {
	db, err := sql.Open("postgres", os.Getenv("QUE_DB_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	q, err := NewWithOptions(Options{DB: db, DBMigrate: true})
	if err != nil {
		t.Fatal(err)
	}

	mu := q.Mutex()
	if _, err := mu.Lock(context.Background(), "idle-closer", 1); err != nil { // connects, starts the idle closer
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("LOCK TABLE goque_jobs IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	// blocks past maxIdle, so the idle closer waits for the mutex, then fails
	ctx, cancel := context.WithTimeout(context.Background(), 3*maxIdle)
	defer cancel()
	if _, err := mu.Lock(ctx, "idle-closer", 1); err == nil {
		t.Fatal("want the blocked Lock to fail")
	}
	_ = tx.Rollback()
	time.Sleep(2 * maxIdle) // the idle closer runs now; it used to panic on a nil cancel

	if _, err := mu.Lock(context.Background(), "idle-closer", 1); err != nil {
		t.Fatal(err)
	}
}

// TestAnIdleConnectionEndsItsSession: closing an idle mutex connection ends
// its database session (nothing is left to clean up in it).
func TestAnIdleConnectionEndsItsSession(t *testing.T) {
	db, err := sql.Open("postgres", os.Getenv("QUE_DB_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	q, err := NewWithOptions(Options{DB: db, DBMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	mu := q.Mutex().(*mutex)
	if _, err := mu.Lock(context.Background(), "idle-session", 1); err != nil {
		t.Fatal(err)
	}
	mu.mu.Lock()
	conn := mu.conn
	var pid int
	err = conn.QueryRowContext(context.Background(), "SELECT pg_backend_pid()").Scan(&pid)
	mu.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * maxIdle)
	for {
		var alive bool
		if err := db.QueryRow("SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)", pid).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %d still alive after the idle connection was closed", pid)
		}
		time.Sleep(maxIdle / 4)
	}
}

// TestAConnectionHoldingLocksIsNeverIdle: the idle closer leaves a connection
// alone while the mutex holds jobs on it, however long it goes unused.
func TestAConnectionHoldingLocksIsNeverIdle(t *testing.T) {
	db, err := sql.Open("postgres", os.Getenv("QUE_DB_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	q, err := NewWithOptions(Options{DB: db, DBMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	qs := fmt.Sprintf("held-%d", time.Now().UnixNano())
	if _, err := q.Enqueue(context.Background(), nil, que.Plan{Queue: qs, Args: que.Args(), RunAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	mu := q.Mutex()
	jobs, err := mu.Lock(context.Background(), qs, 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("want one job but get %d, %v", len(jobs), err)
	}
	id := jobs[0].ID()

	time.Sleep(3 * maxIdle) // the idle closer has run more than once
	var held bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND (classid::bigint << 32 | objid::bigint) = $1 AND objsubid = 1)`, id).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatalf("job %d's lock went with an idle connection while the mutex held it", id)
	}
	if err := mu.Unlock(context.Background(), []int64{id}); err != nil {
		t.Fatal(err)
	}
}
