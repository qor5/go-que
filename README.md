_Note: This repository is a modified version of [https://github.com/tnclong/go-que](https://github.com/tnclong/go-que) to meet our internal customization needs. Thanks to the original repository._

## Que

#### A golang background job queue that uses optimized database lock for reliability and speed.

There are benefits of using Que to route jobs:

- **Transaction** - Controls job along with other changes to your database optional in a transaction.
- **Simplify Architecture** - If you're already using database, a separate queue is another moving part that can break.
- **Safety** - If a golang process dies, the jobs it's working won't be lost, or left in a locked or ambiguous state - they become available for any other worker to pick up according to RetryPolicy as soon as the database sees the process's connection close (see [Database connections](#database-connections) for when that takes longer).
- **Concurrency** - Workers don't block each other. This allows for high throughput with a large number of workers.
- **Uniqueness** - Controls the uniqueness of jobs in same queue.
- **Scheduler** - Schedule jobs uses cron expression.
- **Customization** - Database let you easy to customizate queue suit for your business requirement.

## Requirements

Read these before using go-que.

- **Use the pgx driver; lib/pq is not supported.** Open the database with `github.com/jackc/pgx/v5/stdlib` (which `gorm.io/driver/postgres` uses); go-que is tested only with pgx. lib/pq ignores the context against a database that stops answering (a frozen host, a network partition, a failover whose old address no longer replies), still so in v1.12.3: a `Lock` or `Unlock` called with a deadline waits until the database answers or TCP gives up instead of returning at its deadline, and every other call on the same mutex waits behind it. (`Worker` calls the mutex with `context.Background()`, so a worker on its own waits for the database with either driver.)
- **Connect directly, or through a pooler that pools by session.** A mutex's job locks (PostgreSQL advisory locks) and its helper type and function live in its database session, so it needs the same server connection for its whole life. Do not put go-que behind a pooler that hands each transaction a different server connection: PgBouncer with `pool_mode = transaction` or `statement`, or Amazon RDS Proxy (it pins a session when it sees advisory lock calls, but not ones made inside a function, which is how go-que takes them). Through such a pooler go-que does not work: calls fail and locks outlive the client. If your application must use one, give go-que a direct connection of its own; if it cannot have one, go-que is not for you.
- **Jobs may be performed more than once.** Execution is at least once (see below), so a job must be safe to perform again.

## Database connections

A worker's mutex holds its job locks (PostgreSQL advisory locks) on a database session of its own. Connect directly, or through a pooler that pools by session: a pooler that hands each transaction a different server connection (PgBouncer's `pool_mode = transaction`, say) breaks the locks, and go-que does not work through it.

When a mutex call fails (its connection breaks, a statement fails, the caller's context ends; unlocking ids it never locked is the exception, which only returns `ErrUnlockedJobs`), the mutex drops its session and the database releases all its locks, so other workers may lock and perform those jobs again: execution is at least once. The failing call returns its error and `Worker.Run` returns it once its jobs in flight finish; a new worker, even on the same mutex, connects again. Only `Lock` connects, one call at a time per mutex; `Unlock` never connects and never waits for a `Lock` that is connecting, but it does wait for a `Lock` whose statement is running, which holds the mutex while it runs.

The database releases a dropped session's locks once it notices the connection is gone. When the connection was closed cleanly that is at once; when it is left half-open (a network partition, a host that vanished), the server notices only when TCP keepalive gives up (about two hours with Linux defaults) or when a statement it is running on that session ends. Until then those jobs stay locked: nobody performs them twice, but nobody performs them either. How long that is depends on the database server's operating system and its `tcp_keepalives_idle`, `tcp_keepalives_interval` and `tcp_keepalives_count` settings (0, the default, means the operating system's), and go-que leaves them to whoever runs the database. Shortening them lets other workers take those jobs up sooner, at a price: if a process is cut off from the database but keeps running, a job of its still running when the database gives up on the connection may be locked and performed by another worker at the same time. Keep the time longer than your longest job.

Even with pgx, `Worker` calls the mutex with `context.Background()`, so against a database that stopped answering its calls wait until the database answers or the connection fails: with Go's and Linux's defaults about 15 minutes at most when the database's host is gone, but for as long as the database does not answer when its host is still up. Once a call fails, `Worker.Run` returns the error; to go on, create a new worker (it may use the same mutex), backing off between attempts. `supervisor.StartWorker` does that for you.

## Install

```bash
go get github.com/qor5/go-que
```

## Doc

https://godoc.org/github.com/qor5/go-que

## Quickstart

Start databases:

```bash
docker-compose up
```

If you use PostgreSQL:

```bash
# Migrate database by your favorite tool on production
docker exec -i go-que_postgres_1 psql -U myuser -d mydb < pg/schema.sql
source pg_env
```

Install dependences:

```bash
go get -v ./...
```

Run test:

```bash
go test ./...
```

Example:

example_test.go

## Benchmark

The purpose of benchmark:

- Guide performace improvement.
- Obtain a real WorkerOptions on production with accepted database resource wastage.

Run bm:

```bash
cd bm
# Adjust bm.yaml to simulate your scene.
go run main.go -f bm.yaml
```

Output:

```
using queue "bm-e8b62c5d67bbb33e6771"
                      +-------------------------------------------------
                      |                Duration (ms)                   |
+---------+-----------+------------------------------------------------+
| Method  |    RPS    |  avg   min   p25   p50   p75   p90   p99   max |
+---------+-----------+------------------------------------------------+
| Enqueue |   854.7   |   12     2     6     8    14    23    53   144 |
| Perform |   847.7   |  241    19   105   168   311   566   786   877 |
```
