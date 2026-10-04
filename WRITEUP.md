# Write-up

Stack: Go, Fiber, MySQL 8 (a single database), deployed on Render with a managed MySQL on Aiven.

## 1. The atomic decision

The decision "is this seat free, and if so take it" is one SQL statement, inside one transaction:

```sql
UPDATE show_seats SET status='confirmed', reservation_id=?
WHERE show_id=? AND seat_label IN (...) AND status='available';
```

It is race-free because InnoDB takes an exclusive row lock on each row the `UPDATE` touches and holds it until
commit. A second transaction aiming at the same row waits, then re-evaluates the `WHERE` against the committed
value, sees `confirmed`, and matches nothing. The check and the write cannot be separated by another request, so
there is never a read-then-write gap.

**Multi-seat requests are all-or-nothing.** I compare `RowsAffected` with the number of seats requested. If it is
short, the transaction rolls back, which also undoes the reservation row and the limit counter, and the caller gets
a 409 listing the seats that were taken. No partial state is ever visible to another transaction.

**Deadlock avoidance.** Every request takes locks in the same order: the reservation row (unique key), then the
`user_show` row, then the seat rows. Seats are sorted as plain strings before the statement runs. The column uses
a binary collation, so that order matches the primary key order InnoDB scans in, and two requests for `[A1,A2]` and
`[A2,A1]` lock in the same order and queue instead of deadlocking. Cancel uses the same order. Sorting reduces
deadlocks, but I do not rely on it: MySQL errors 1213 (deadlock) and 1205 (lock wait timeout) are retried inside
the store with jittered exponential backoff (up to 8 attempts), and counted in `store_retries_total`. Retries run a
fresh transaction, since InnoDB has already rolled the failed one back.

**Per-user limit.** One conditional update on a counter row keyed `(show_id, user_id)`:

```sql
UPDATE user_show SET seats_held = seats_held + ?
WHERE show_id=? AND user_id=? AND seats_held + ? <= ?;
```

Zero rows affected means over the limit. Parallel requests from one user queue on that row, and each one evaluates
the limit against the latest committed value, so ten parallel reserves on a limit of 4 end with exactly 4. The
increment is inside the transaction, so a request that later fails on seats gives the capacity back. The counter
row is created with `INSERT IGNORE` in autocommit mode _before_ `BEGIN`, on purpose: inside the transaction it
would take a shared lock that has to be upgraded to exclusive by the following update, which deadlocks when two
requests from the same user collide, and it would make each request hold two pooled connections at once. The only
residue of a failed request is a harmless row with `seats_held = 0`.

## 2. Idempotency

- **Where the key lives:** the `reservations` table, with `UNIQUE (user_id, idempotency_key)`. Keys are scoped per
  user, so two users can use the same string independently.
- **Exactly once:** the reservation row is the first statement in the transaction. If two requests carry the same
  key, the second waits on the unique index entry. If the first commits, the second gets error 1062, rolls back, and
  reads the original reservation. If the first rolls back (for example the seats were taken), the second simply
  proceeds with its own attempt. The read-after-1062 returns "not found" only in a narrow race, which is retried.
- **Same key, different body:** every row stores a SHA-256 of the show id plus the _sorted_ seat list. A retry with
  the same seats in another order is the same request. A different seat set, or the same key on a different show,
  has a different hash and gets 409.
- **What a replay returns:** the original reservation, status 201, with the header `Idempotent-Replay: true`. It
  moves nothing: no extra seat, no counter change. A request that was declined stores nothing, so retrying it is
  evaluated again from scratch.

## 3. Holds and expiry

I chose the explicit cancel model. Reserve returns `confirmed` immediately, so there is no payment step and nothing
to hold, and the `held` count is always 0 (the invariant still holds: `available + held + confirmed == total_seats`).

`POST /reservations/{id}/cancel` locks the reservation row (`SELECT ... WHERE id=? AND user_id=? FOR UPDATE`), so
the owner check is part of the lookup and someone else's reservation looks exactly like a missing one (404). It
then marks the reservation cancelled, decrements the user's counter, and frees seats with
`WHERE reservation_id = ? AND status = 'confirmed'`. Because the match is on the reservation id, a stale or
repeated cancel can never free a seat that has since been rebooked by someone else. A repeated cancel returns 200
and changes nothing.

## 4. Consistency versus availability under a partition

The service has one MySQL primary, so under a partition it chooses consistency. If the application cannot reach the
database, requests fail instead of guessing about a seat, and `/readyz` returns 503 so the platform stops sending
traffic. I accept that those failures are 5xx. The "zero 5xx" property holds while the database is reachable. A
decline is always a 409 or a 4xx, never an error.

Two details that follow from this: a commit whose acknowledgement is lost can leave the client not knowing the
outcome, which is exactly the case idempotency keys exist for, since the retry returns the original reservation.
`/readyz` and the seat gauge use a separate single-connection pool, so health checks do not queue behind customer
traffic and cannot trigger a restart in the middle of a burst, while still failing closed when the database is down.

## 5. Observability: what would page me at 2am

- **Any 5xx** (`http_requests_total{status=~"5.."}`), and any `store_retries_total{reason="exhausted"}`. Both mean a
  request failed that should have been a clean outcome.
- **Readiness failing**, i.e. the database is unreachable.
- **Connection pool saturation:** `go_sql_wait_count_total` and `go_sql_wait_duration_seconds_total` (label
  `db_name="seatdb"`), and `go_sql_in_use_connections` near `DB_MAX_CONNS`. The managed database allows 46
  connections, so the pool is 15 per instance, leaving room for a redeploy overlap.
- **p99 latency** of `http_request_duration_seconds` for the reserve route, and a rising `store_retries_total`
  rate (deadlocks or lock timeouts), which would mean contention is up.
- **Reconciliation drift:** `sum(seats)` must equal the sum of `total_seats` over all shows (a SQL query, since
  there is no total-seats metric), and over any window the change in `seats{status="confirmed"}` must equal the
  confirmed seats minus the cancelled ones. The burst tool asserts the second one on every run.

Logs are one JSON line per request with a request id (echoed in `X-Request-ID`), user, route, status, latency and
outcome. Honest limits: counters reset when the process restarts (so the burst tool compares deltas), and the seat
gauge is aggregated over all shows. `GET /shows/{id}` is the per-show view.

## 6. AI usage (directed versus decided)

I used Claude (Anthropic) as a pair-programmer and reviewer. Here is the split.

**What I decided and owned:** the approach, the tech stack (Go, Fiber, MySQL, zerolog), the platforms (Render and
Aiven), the repo, the deployment, and debugging the Aiven connection and the Render environment.

**What I wrote:** the create-show, reserve and cancel handlers and store functions, and the first versions of
several tests. Claude reviewed each draft and I fixed what it found; the cancel function in the repo is Claude's
corrected version of mine. I ran every test and the mutation check myself.

**Schema:** I designed the schema (`shows`, one row per seat in `show_seats`, `reservations` and the per-user counter
table). Claude helped with the per-user seat limit: the `user_show` counter and its conditional update.

**Partial requests:** I asked for guidance. Claude recommended all-or-nothing, and I adopted it once I could explain
how it holds under concurrency.

**Where Claude helped most:** observability and metrics, the burst script, the test setup and most of the remaining
test bodies, the idempotency and retry design (I wrote the code from outlines), the snapshot read behind
`GET /shows/{id}`, and the Dockerfile, TLS, migration and JWT code. I ran, read and debugged all of it, and I will go
through it again before the interview.

**Bugs found by review and tests:**

- the duplicate-seat check inserted into the set before checking it, so every request looked like a duplicate;
- in cancel, a dropped `Scan` error and a missing owner filter let another user cancel a reservation (a test caught it);
- the per-user counter `INSERT` ran on a second pooled connection while the transaction held the first, which would
  exhaust the pool under load;
- Fiber reuses request buffers, which corrupted Prometheus label values and made `/metrics` return 500.

**How I verified it:** concurrency tests against a real MySQL (hot seat, same key in parallel, per-user limit,
opposite seat order, and a 2,000-request stampede with the invariant polled during the burst), run 20 times in a row
(the stampede 5 times); one mutation check, where moving the seat update from `tx` to `db` made the partial-request
test fail as it should; and the burst tool against both a local stack and the live service.
