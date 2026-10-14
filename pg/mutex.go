package pg

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qor5/go-que"
)

type mutex struct {
	db *sql.DB

	mu     sync.Mutex
	conn   *sql.Conn
	active time.Time
	cancel context.CancelFunc
	ids    map[int64]bool // locked on conn
	// dialing is closed when the connecting call in flight is done; nil when
	// none is
	dialing chan struct{}
	// lost were locked on a connection since dropped: their locks are gone,
	// but their callers still have them, so they are not locked again until
	// unlocked.
	lost map[int64]bool
}

type releaseConn func(error)

func (m *mutex) grabConn(ctx context.Context) (*sql.Conn, releaseConn, error) {
	m.mu.Lock()
	for m.conn == nil {
		if dialing := m.dialing; dialing != nil {
			// another call is connecting: wait for it rather than open a
			// connection of our own
			m.mu.Unlock()
			select {
			case <-dialing:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			m.mu.Lock()
			continue
		}
		// connect without holding m.mu: a database that does not answer
		// (and a driver that ignores ctx while connecting) must not block
		// the calls that need no connection, such as unlocking lost jobs
		dialing := make(chan struct{})
		m.dialing = dialing
		m.mu.Unlock()
		conn, err := m.connect(ctx)
		m.mu.Lock()
		m.dialing = nil
		close(dialing)
		if err != nil {
			m.mu.Unlock()
			return nil, nil, err
		}
		m.conn = conn
		var cctx context.Context
		cctx, m.cancel = context.WithCancel(context.Background())
		go m.closeIdleConn(cctx)
	}
	m.active = time.Now()
	return m.conn, m.unlockCondReleaseConn, nil
}

// connect opens a connection of the mutex's own and sets it up.
func (m *mutex) connect(ctx context.Context) (*sql.Conn, error) {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if err = setupConn(ctx, conn); err != nil {
		m.closeConn(conn)
		return nil, err
	}
	return conn, nil
}

func (m *mutex) unlockCondReleaseConn(err error) {
	defer m.mu.Unlock()
	if err == nil {
		return
	}

	// the session is in doubt: drop it, and every lock with it; the next
	// call connects again
	m.cancel()
	m.cancel = nil
	if err != driver.ErrBadConn {
		m.closeConn(m.conn)
	}
	m.conn = nil
	for id := range m.ids {
		m.lost[id] = true
	}
	m.ids = make(map[int64]bool)
}

var maxIdle = 10 * time.Second

func (m *mutex) closeIdleConn(ctx context.Context) {
	ticker := time.NewTicker(maxIdle)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.tryCloseIdleConn(ctx) {
				return
			}
		}
	}
}

func (m *mutex) tryCloseIdleConn(ctx context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return true // its connection was dropped while it waited
	}
	if len(m.ids) != 0 {
		return false
	}
	if m.active.Add(maxIdle).After(time.Now()) {
		return false
	}

	m.cancel()
	m.cancel = nil
	m.closeConn(m.conn)
	m.conn = nil
	return true
}

func setupConn(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, setupConnSQL)
	return err
}

const lockJobs = `WITH RECURSIVE jobs AS (
    SELECT (jb).*, locks.locked, locks.remaining
    FROM (
             SELECT jb
             FROM public.goque_jobs AS jb
             where queue = $1::text
               AND NOT id = ANY ($2::bigint[])
               AND run_at <= now()
               AND done_at IS NULL AND expired_at IS NULL
             ORDER BY run_at, id
             LIMIT 1
         ) AS jb_t
             JOIN LATERAL (SELECT *
                           FROM pg_temp.goque_lock_and_decrease_remaining($3, jb)) AS locks ON TRUE
    UNION ALL
    (
        SELECT (jb).*, locks.locked, locks.remaining
        FROM (
                 SELECT remaining,
                        (
                            SELECT jb
                            FROM public.goque_jobs AS jb
                            WHERE queue = $1::text
                              AND NOT id = ANY ($2::bigint[])
                              AND run_at <= now()
                              AND done_at IS NULL AND expired_at IS NULL
                              AND (run_at, id) >
                                  (jobs.run_at, jobs.id)
                            ORDER BY run_at, id
                            LIMIT 1
                        ) AS jb
                 FROM jobs
                 WHERE jobs.remaining != 0
                 LIMIT 1
             ) AS jb_t
                 JOIN LATERAL (SELECT *
                               FROM pg_temp.goque_lock_and_decrease_remaining(remaining, jb)) AS locks ON TRUE
    )
)
SELECT *
FROM jobs
WHERE locked`

func (m *mutex) Lock(ctx context.Context, queue string, count int) (jobs []que.Job, err error) {
	if count <= 0 {
		return nil, nil
	}

	var conn *sql.Conn
	var release releaseConn
	conn, release, err = m.grabConn(ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	defer func() {
		if err != nil {
			// a failed Lock hands out no job; their locks go with the connection
			for _, id := range ids {
				delete(m.ids, id)
			}
		}
		release(err)
	}()
	jobs, ids, err = m.lockInMux(ctx, conn, queue, count)
	if err != nil {
		return jobs, err
	}
	var validIDs map[int64]bool
	validIDs, err = m.verifyJobs(ctx, conn, ids)
	if err != nil {
		return nil, err
	}
	if len(jobs) == len(validIDs) {
		return jobs, nil
	}
	var i int
	invalidIDs := make([]int64, 0, len(ids)-len(validIDs))
	for _, job := range jobs {
		id := job.ID()
		if validIDs[id] {
			jobs[i] = job
			i++
		} else {
			invalidIDs = append(invalidIDs, id)
		}
	}
	if _, err = m.unlockInMux(ctx, conn, invalidIDs); err != nil { // all held: Lock just locked them
		return nil, err
	}
	return jobs[:i], nil
}

func (m *mutex) lockInMux(ctx context.Context, conn *sql.Conn, queue string, count int) (jobs []que.Job, ids []int64, err error) {
	var rows *sql.Rows
	rows, err = conn.QueryContext(ctx, lockJobs, queue, m.idsAsString(), count)
	if err != nil {
		return nil, nil, err
	}
	var doneAt, exipredAt sql.NullTime
	var locked bool
	var remaining int
	defer rows.Close()
	for rows.Next() {
		var jb job
		rp := (*jsonRetryPolicy)(&jb.plan.RetryPolicy)
		var uniqueID sql.NullString
		err = rows.Scan(
			&jb.id, &jb.plan.Queue, &jb.plan.Args, &jb.plan.RunAt, rp, &doneAt, &exipredAt,
			&jb.retryCount, &jb.lastErrMsg, &jb.lastErrStack,
			&uniqueID, &jb.plan.UniqueLifecycle,
			&locked, &remaining,
		)
		if err != nil {
			return nil, ids, err
		}
		if doneAt.Valid {
			err = fmt.Errorf("get a job(%v) has done_at=%s", jb.id, doneAt.Time.String())
			panic(err)
		}
		if exipredAt.Valid {
			err = fmt.Errorf("get a job(%v) has expired_at=%s", jb.id, doneAt.Time.String())
			panic(err)
		}
		if !locked {
			err = fmt.Errorf("get a job(%v) has locked=%v", jb.id, locked)
			panic(err)
		}
		jb.db = m.db
		if uniqueID.Valid {
			jb.plan.UniqueID = &uniqueID.String
		}
		m.ids[jb.id] = true
		jobs = append(jobs, &jb)
		ids = append(ids, jb.id)
	}
	if err = rows.Err(); err != nil {
		return nil, ids, err
	}
	return jobs, ids, nil
}

func (m *mutex) verifyJobs(ctx context.Context, conn *sql.Conn, ids []int64) (map[int64]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err := conn.QueryContext(ctx, fmt.Sprintf(verifyJobsSQL, inInt64Array(ids)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	validIDs := make(map[int64]bool)
	for rows.Next() {
		var id int64
		err := rows.Scan(&id)
		if err != nil {
			return nil, err
		}
		validIDs[id] = true
	}
	return validIDs, rows.Err()
}

func inInt64Array(ids []int64) string {
	var buf []byte
	buf = strconv.AppendInt(buf, ids[0], 10)
	for _, id := range ids[1:] {
		buf = append(buf, ',')
		buf = strconv.AppendInt(buf, id, 10)
	}
	return string(buf)
}

// idsAsString lists the jobs Lock must skip: the ones this mutex holds, and
// the lost ones still with their callers.
func (m *mutex) idsAsString() string {
	if len(m.ids)+len(m.lost) > 0 {
		b := make([]byte, 1, 1+(len(m.ids)+len(m.lost))*2)
		b[0] = '{'
		for _, ids := range []map[int64]bool{m.ids, m.lost} {
			for id := range ids {
				b = strconv.AppendInt(b, id, 10)
				b = append(b, ',')
			}
		}
		b[len(b)-1] = '}'
		return string(b)
	}
	return "{}"
}

func (m *mutex) Unlock(ctx context.Context, ids []int64) (err error) {
	if len(ids) == 0 {
		return nil
	}

	// one hold of m.mu from forgetting lost ids to unlocking held ones: a
	// failure in between would make held ids lost, leaving them excluded
	// for good and reported as never locked
	m.mu.Lock()
	if ids = m.forgetLostInMux(ids); len(ids) == 0 {
		m.mu.Unlock()
		return nil
	}
	// Unlock never connects: without a connection the mutex holds nothing,
	// so every id left is reported as never locked
	m.active = time.Now()
	conn := m.conn
	var notHeld bool
	defer func() {
		m.unlockCondReleaseConn(err) // a failure drops the session; unknown ids alone do not
		if err == nil && notHeld {
			err = &que.ErrQueue{Err: que.ErrUnlockedJobs}
		}
	}()
	notHeld, err = m.unlockInMux(ctx, conn, ids)
	return
}

// forgetLostInMux drops duplicates, and the lost ones among ids: their locks
// went with an earlier connection, so unlocking them needs none.
func (m *mutex) forgetLostInMux(ids []int64) []int64 {
	rest := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if m.lost[id] {
			delete(m.lost, id)
		} else {
			rest = append(rest, id)
		}
	}
	return rest
}

// unlockInMux unlocks the ids this mutex holds, and reports ids it never
// locked.
func (m *mutex) unlockInMux(ctx context.Context, conn *sql.Conn, ids []int64) (notHeld bool, err error) {
	held := make([]int64, 0, len(ids))
	for _, id := range ids {
		if m.ids[id] {
			held = append(held, id)
		} else {
			notHeld = true
		}
	}
	if len(held) > 0 {
		err = m.unlockHeld(ctx, conn, held)
	}
	return notHeld, err
}

func (m *mutex) unlockHeld(ctx context.Context, conn *sql.Conn, ids []int64) (err error) {
	defer func() {
		if err != nil {
			// their callers are done with them; their locks go with the connection
			for _, id := range ids {
				delete(m.ids, id)
			}
		}
	}()
	var b strings.Builder
	b.WriteString("SELECT pg_advisory_unlock(v.i) FROM (VALUES (")
	b.WriteString(strconv.FormatInt(ids[0], 10))
	for _, id := range ids[1:] {
		b.WriteString("), (")
		b.WriteString(strconv.FormatInt(id, 10))
	}
	b.WriteString(")) v (i)")

	var rows *sql.Rows
	rows, err = conn.QueryContext(ctx, b.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	var notLocked []int64
	var i int
	for rows.Next() {
		var unlocked bool
		err = rows.Scan(&unlocked)
		if err != nil {
			return err
		}

		delete(m.ids, ids[i])
		if !unlocked {
			notLocked = append(notLocked, ids[i])
		}
		i++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(notLocked) > 0 {
		return &que.ErrQueue{Err: que.ErrUnlockedJobs}
	}
	return nil
}

// closeConn closes the physical connection, which ends its session: its
// advisory locks and pg_temp objects go with it, so nothing is cleaned up
// first (a statement could hang on a database that stopped answering).
func (m *mutex) closeConn(conn *sql.Conn) {
	conn.Raw(func(driverConn interface{}) error {
		return driver.ErrBadConn
	})
}

func (q *queue) query(tx *sql.Tx) func(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if tx != nil {
		return tx.QueryContext
	}
	return q.db.QueryContext
}

type jsonRetryPolicy que.RetryPolicy

func (rp jsonRetryPolicy) Value() (driver.Value, error) {
	return json.Marshal(rp)
}

func (rp *jsonRetryPolicy) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return errors.New("jsonRetryPolicy: type assertion to []byte failed")
	}

	return json.Unmarshal(b, &rp)
}
