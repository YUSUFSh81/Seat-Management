# Seat reservation at scale

A JSON HTTP API for creating shows and reserving seats under heavy concurrent load.
Go + Fiber + MySQL (a single database). Money is integer paise, never floats.

- **Live URL:** https://seat-management-yv11.onrender.com
- **Write-up:** [WRITEUP.md](WRITEUP.md)
- **Burst results from the live service:** [docs/burst-live-500.txt](docs/burst-live-500.txt)

## Run locally

Requirements: Docker for the stack. Go 1.26 for `cmd/token` and `./burst.sh` (both use `go run`).

```bash
docker compose up --build
curl localhost:8080/readyz        # {"status":"ready"}
```

Compose starts MySQL 8 and the API (`JWT_SECRET=dev-secret`). Migrations run automatically at startup.

Tests run against a real MySQL (the interesting behaviour is in the locks, so there are no mocks):

```bash
docker compose up db -d
docker compose exec db mysql -uroot -proot -e "CREATE DATABASE IF NOT EXISTS seats_test;"
TEST_DB_DSN='root:root@tcp(127.0.0.1:3307)/seats_test?parseTime=true' go test ./internal/store -count=1
```

## Auth

HS256 JWT with claims `sub` (user id) and `role` (`admin` or `user`). Identity comes only from the
verified token, never from the request body. A `user_id` in a body is ignored.

```bash
export JWT_SECRET=dev-secret
ADMIN_TOKEN=$(go run ./cmd/token -user boss  -role admin)
USER_TOKEN=$(go run ./cmd/token  -user alice -role user)
```

Live service: the signing secret is deliberately not in this repo. It is in the submission message. With it you can mint tokens exactly as above, and ./burst.sh uses the same secret to sign tokens for its test users:

```bash
export JWT_SECRET=<secret from the submission message>
ADMIN_TOKEN=$(go run ./cmd/token -user grader-admin -role admin -ttl 72h)
USER_TOKEN=$(go run ./cmd/token  -user grader-user  -role user  -ttl 72h)
curl -s https://seat-management-yv11.onrender.com/shows/1?include_seats=false
```

## Endpoints

| Method and path                  | Who        | Notes                                                                                                  |
| -------------------------------- | ---------- | ------------------------------------------------------------------------------------------------------ |
| `POST /shows`                    | admin      | Creates a show and all its seats as `available`. Seats are inserted in batches inside one transaction. |
| `POST /shows/{id}/reserve`       | user       | Reserves seats. Idempotency key in the body or the `Idempotency-Key` header.                           |
| `POST /reservations/{id}/cancel` | owner only | Releases the seats. Repeating a cancel returns 200.                                                    |
| `GET /shows/{id}`                | public     | Counts and per-seat status. `?include_seats=false` returns counts only.                                |
| `GET /healthz`                   | public     | Liveness. Never touches the database.                                                                  |
| `GET /readyz`                    | public     | Readiness. Pings the database and returns 503 when it is unreachable.                                  |
| `GET /metrics`                   | public     | Prometheus metrics.                                                                                    |

```bash
curl -s -X POST $BASE/shows -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"friday-night","seats":["A1","A2","A3"],"price_paise":25000}'

curl -s -X POST $BASE/shows/1/reserve -H "Authorization: Bearer $USER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"seats":["A1","A2"],"idempotency_key":"order-123"}'
# 201 {"reservation_id":"1","show_id":"1","user_id":"alice","seats":["A1","A2"],"amount_paise":50000,"status":"confirmed"}

curl -s -X POST $BASE/reservations/1/cancel -H "Authorization: Bearer $USER_TOKEN"
```

### Responses

| Status    | Meaning                                                                                                                       |
| --------- | ----------------------------------------------------------------------------------------------------------------------------- |
| 201       | Reserved, or a replay of the same request (replays add the header `Idempotent-Replay: true`).                                 |
| 400       | Malformed input: bad JSON, empty or invalid seat label, duplicate labels, missing key, bad show id.                           |
| 401 / 403 | Missing or invalid token / not an admin.                                                                                      |
| 404       | Unknown show, or a reservation that does not exist or is not yours.                                                           |
| 409       | A clean decline: seats unavailable (the body lists them), per-user limit, or idempotency key reused with a different request. |
| 5xx       | Only real server faults (for example the database being down). A decline is never a 5xx.                                      |

## One-command burst

```bash
JWT_SECRET=<the server's secret> ./burst.sh <BASE_URL> [-n 20000] [-conc 100]
# or: JWT_SECRET=... go run ./cmd/burst -url <BASE_URL>
# or: JWT_SECRET=... make burst BASE_URL=<BASE_URL>
```

It creates a fresh show and runs, in order: a hot-seat storm (500 users, one seat), a mixed stampede
(`-n` requests over 20 hot seats plus cold seats, with double-clicks and retries), one user firing 10
parallel reserves, the same key fired 20 times in parallel, a key reused with different seats, a spoofed
`user_id`, and cancel, rebook, repeat cancel and non-owner cancel. It prints the outcome distribution,
then asserts:

- exactly one 201 per contested seat, everything else 409
- zero 5xx and zero network errors, on the client and in the server's own metrics
- the invariant `available + held + confirmed == total_seats`, polled every 100 ms during the burst and checked at the end
- the confirmed seats on the server equal the seats the API confirmed minus those cancelled
- the metric deltas (confirmed, cancelled, seats confirmed, declined by reason) match what the client observed

It exits non-zero if any check fails. It uses unique users and keys on every run, so it can be re-run on a used
database. Run it while nobody else is using the service, otherwise the metric deltas include their traffic.

## Observability

- **Metrics** (`/metrics`): `reservations_confirmed_total`, `seats_confirmed_total`,
  `reservations_cancelled_total`, `reservations_declined_total{reason}` with reasons `seat_taken`,
  `per_user_limit`, `idempotent_replay` and `idempotency_conflict`, `store_retries_total{reason}`,
  `http_requests_total{method,route,status}`, `http_request_duration_seconds`, `seats{status}` (read from the
  database at scrape time, cached for 1 second) and `go_sql_*` connection pool metrics (label `db_name="seatdb"`).
- **Logs:** one JSON line per request (zerolog) with `request_id` (accepts `X-Request-ID`, echoes it back),
  route, status, latency, `user_id` and `outcome`. Probes (`/healthz`, `/readyz`, `/metrics`) are not logged.
  The live logs are in the Render dashboard, which is private to my account, so there is no public link. An excerpt
  of real lines captured from the live service during the burst is in `docs/sample-logs.txt`. To follow one request
  through the logs, search for its `request_id`, which is also returned in the `X-Request-ID` response header.

## Decisions

- **Partial requests are all-or-nothing.** If any requested seat is unavailable the whole request gets a 409
  naming the unavailable seats, and nothing is reserved.
- **No holds.** Reserve confirms immediately, so the release model is the explicit cancel endpoint.
- **Idempotency keys are scoped per user.** A replay returns the original reservation with 201. The same key
  with different seats, or on a different show, returns 409. A declined request stores nothing, so retrying it
  is simply evaluated again.
- **Seat labels** are trimmed and uppercased, `A-Z`, `0-9` and `-`, 1 to 16 characters.
- **Per-user limit** is per show (default 4, settable with `per_user_limit` on create).
- A requested label that does not exist in the show counts as unavailable, but it is not listed in the 409 body.

## Cold start

The first request after the service has been idle took about 13.6 s on this plan (Render's free tier spins the
service down when idle). It comes up healthy on its own: it retries the database connection for up to 60 seconds
at startup and runs its own migrations. `./burst.sh` waits for `/readyz` before it starts (up to 2 minutes), so it
absorbs the cold start. If you script against the service yourself, hit `/healthz` once first.

## Configuration

| Variable       | Required | Default | Purpose                                                                    |
| -------------- | -------- | ------- | -------------------------------------------------------------------------- |
| `DB_DSN`       | yes      |         | MySQL DSN, with `parseTime=true`                                           |
| `JWT_SECRET`   | yes      |         | HS256 signing secret                                                       |
| `PORT`         | no       | 8080    | Listen port (Render injects it)                                            |
| `DB_MAX_CONNS` | no       | 15      | Main pool size (the managed database allows 46 connections)                |
| `DB_CA_CERT`   | no       |         | PEM text of the CA, when the database requires TLS (DSN uses `tls=custom`) |

## Layout

```
  cmd/api/                  the server
  cmd/burst/                load and reconciliation tool
  cmd/token/                mints test tokens
  internal/auth/            JWT middleware
  internal/obs/             metrics and request logging
  internal/store/           all SQL, migrations, tests
  internal/store/handlers/  HTTP handlers
```

## Results

Live burst output: [docs/burst-live-500.txt](docs/burst-live-500.txt). Metrics snapshot taken after it:
[docs/metrics-after-burst.txt](docs/metrics-after-burst.txt).

500-request burst with 50 in flight (1,147 reserve calls in total): all 26 checks passed. 96 confirmed, 38
idempotent replays, 1,003 seat-taken, 9 over-limit, 1 key conflict, 0 5xx, 0 network errors. Latency p50 1.09 s,
p95 8.4 s, p99 8.6 s: requests queue for the 15 database connections (the free database allows 46), and each
reserve makes about six round trips to a remote database.
