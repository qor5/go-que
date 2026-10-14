package pg

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
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
	defer db.Close()
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
	defer tx.Rollback()
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
