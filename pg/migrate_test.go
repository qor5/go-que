package pg_test

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/qor5/go-que/pg"
)

// Instances that start together on a fresh database all migrate it: "if not
// exists" does not keep two sessions from creating the same table at once.
func TestMigrateConcurrentlyOnAFreshDatabase(t *testing.T) {
	source := os.Getenv("QUE_DB_SOURCE")
	admin, err := sql.Open("postgres", source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	for round := range 5 {
		schema := fmt.Sprintf("migrate_%d_%d", time.Now().UnixNano(), round)
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
		u, err := url.Parse(source)
		if err != nil {
			t.Fatal(err)
		}
		v := u.Query()
		v.Set("search_path", schema)
		u.RawQuery = v.Encode()
		db, err := sql.Open("postgres", u.String())
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 8)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				<-start
				errs <- pg.Migrate(db)
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		_ = db.Close()
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: want every instance to migrate but get %v", round, err)
			}
		}
	}
}
