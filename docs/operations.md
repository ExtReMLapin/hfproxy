# Operations runbook

## First start

A new instance has no administrator. Opening the console shows a setup page that creates the first one and signs them in.

- **Setup code.** The page asks for a code, because a fresh instance may be reachable before anyone has claimed it. When no administrator exists, hfproxy prints the code on its standard error output at start-up, whatever the log level (`docker compose logs hfproxy`, `kubectl logs deploy/hfproxy`). A new random code is generated at each start.
- **Fixed code.** Set `HFPROXY_SETUP_TOKEN` (16 to 256 characters, or `HFPROXY_SETUP_TOKEN_FILE`) to choose the code yourself. It is then never printed. Use this with several replicas, which would otherwise each print a different code, and for automated installs.
- **Guessing.** Attempts share one limit (10 per minute) across all clients; a wrong code is logged as a warning with the client address.
- **Only once.** The setup is refused as soon as an active administrator exists, even if two people submit at the same time: the check and the creation happen under one database lock. The setup page and endpoints then answer "already completed".
- **CLI.** `hfproxy admin create-user` still works and also closes the setup page.
- **Recovery.** If every administrator was disabled directly in the database, the setup page reappears within 30 seconds and a code is printed on the output at that moment.

## Logging

Logs are JSON lines on standard output. The level is `debug`, `info`, `warn` (or `warning`) or `error`; the default is `info`.

- **Set it** with `HFPROXY_LOG_LEVEL`, or in the console under **Settings → Observability → Log level**. A level saved in the console takes effect immediately, without a restart, and wins over the environment unless `log_level` is listed in `HFPROXY_LOCKED_SETTINGS`. Each change is logged (`log level changed`) and recorded in the audit log with the other settings.
- **error**: server errors, with the underlying cause (`request failed`), failed audit writes, and every request that ended in a 5xx.
- **warn**: refused or invalid requests (400, 403, 409, 429…) with the reason in the `problem` field, denied access with the user and role, failed sign-ins (client address and provider only), upstream failures such as an unreachable Hub or object store, and preload files that could not be cached.
- **info**: one line per request (`http request`: route, status, duration, bytes, user, token), one `audit event` line per administrative action (who, what, which target), and start-up and maintenance summaries.
- **debug**: authentication decisions, cache searches, preload progress per file, and the health probes and static files that are otherwise hidden. Expect a lot of output; use it for an investigation, then go back to `info`.

Never logged: passwords, tokens, secrets, request bodies, query strings, or the user name typed at a failed sign-in (people sometimes type their password there; a capped copy is kept in the audit table). Use the `request_id` of a line, also returned in the `X-Request-ID` header and in error responses, to follow one request.

## Health and readiness

- `/healthz` answers while the process runs. `hfproxy healthcheck` probes it and is the container health check.
- `/readyz` answers 200 only when PostgreSQL is reachable, the schema is at the version this build expects, and the object-storage bucket is reachable. Route traffic on `/readyz`.

## Upgrades and rollbacks

1. Back up PostgreSQL (see below).
2. Run `hfproxy migrate status`, then `hfproxy migrate up`, or start one instance with `HFPROXY_AUTO_MIGRATE=true`.
3. Roll out the new image. A new build refuses to start until the schema is at its version, and `/readyz` stays unavailable until then.

Migrations are embedded SQL files (`internal/db/migrations`, one per version) and only add tables, columns and indexes while a release depends on the old shape, so the previous image still starts after a migration. To roll back, redeploy the previous image; do not remove migration rows by hand.

One rollback limit: from 0.0.4 on, access tokens move to a derived key the first time they are used, sessions are created under it, and saved settings are rewritten in a keyed format when they are saved or rotated. An older release cannot verify those: its users would have to sign in again and create new tokens, and it cannot read re-saved settings. Back up the database before upgrading, and treat a rollback past 0.0.4 as a restore from that backup.

## Rotating the session secret

`HFPROXY_SESSION_SECRET` is the root of three separate keys: one hashes access tokens and sessions, one encrypts the saved settings, and one only names the key. To change it without logging everyone out or losing the saved settings:

1. Generate a new secret (`openssl rand -base64 48`) and store it with your other secrets.
2. On every instance set `HFPROXY_SESSION_SECRET` to the new value and `HFPROXY_SESSION_SECRET_PREVIOUS` to the old one (up to four old values, separated by commas or newlines; `HFPROXY_SESSION_SECRET_PREVIOUS_FILE` works too). Roll the instances out. Existing tokens, sessions and saved settings keep working; new ones use the new secret.
3. Run `hfproxy admin rotate-secret` once. It re-encrypts the saved settings under the new secret and prints how many active tokens and sessions are still stored under an older secret.
4. An access token moves to the new secret the first time it is used. Sessions end on their own. Revoke tokens nobody uses any more with `hfproxy admin token revoke`, or wait until `rotate-secret` reports 0.
5. When both counts are 0, remove `HFPROXY_SESSION_SECRET_PREVIOUS` and roll out again. From then on the old secret is useless.

If you forget step 2 and only replace `HFPROXY_SESSION_SECRET`, every token and session created under the old secret is refused (clients get 401). If settings were saved from the console, hfproxy also refuses to start, because they cannot be decrypted. Either way, put the old value in `HFPROXY_SESSION_SECRET_PREVIOUS` to recover; nothing is lost.

Tokens created before this key separation existed (hashed straight from the secret) are still recognised, and move to the derived key on first use as well.

## Backup and restore

- Back up PostgreSQL and the object-storage bucket **together**. The catalog references blobs by hash; a database restored without the matching objects lists files as cached that no longer exist, so downloads of them fail with 502 until their entries are purged and fetched again, and objects that have no row stay in the bucket until you remove them. `hfproxy cache verify` finds the mismatches and `hfproxy cache purge` drops the stale entries.
- Back up `SESSION_SECRET` (and any `SESSION_SECRET_PREVIOUS`) with your secrets. Without them, saved settings cannot be decrypted and every token and session is invalid.
- Test a restore: start a copy against the restored database and bucket, check `/readyz`, and download one cached file.
- After a restore, run `hfproxy cache verify` to re-read objects and check their SHA-256.

## Incidents

| Symptom | Likely cause | Action |
| --- | --- | --- |
| `/readyz` 503 | PostgreSQL or the bucket is unreachable, or the schema is behind | Check the database, credentials and bucket; run `hfproxy migrate status` |
| Clients get 502 with `Upstream unavailable` | Hub outage or rate limiting | Cached files and cached metadata (`X-Cache: STALE`) still serve; check `hfproxy_upstream_errors_total` and `hfproxy_upstream_breaker_opened_total`; wait or use `HFPROXY_HF_PROXY_URL` |
| Log line "upstream circuit opened" | Five consecutive Hub failures | Expected during an outage: requests fail fast for the cooldown, then one probe retries. If it repeats while the Hub is healthy, check your egress path and proxy |
| Clients get 503 `Cache fill capacity exhausted` | More simultaneous fills than `HFPROXY_CACHE_FILL_CONCURRENCY` | Retry; raise the limit if disk and bandwidth allow |
| Clients get 507 | Staging volume too small for the file | Enlarge `HFPROXY_STAGING_DIR` or lower concurrency |
| `/readyz` 503 with `"reason":"staging_space"` | Less free staging space than `HFPROXY_STAGING_MIN_FREE_BYTES` | Free or enlarge the staging volume; the instance is back in rotation once the space returns |
| Log "the cache is over its size limit but what remains is pinned" | Pinned files alone exceed the quota | `hfproxy cache pins` shows what is pinned; unpin what is no longer essential or raise `HFPROXY_CACHE_MAX_TOTAL_SIZE` |
| Storage errors in logs, `hfproxy_storage_errors_total` rising | Object storage outage or bad credentials | Restore storage access; cached reads fail until then |
| Cache keeps growing | No quota set | Set `HFPROXY_CACHE_MAX_TOTAL_SIZE`; use `hfproxy cache purge` (dry run by default) |
| Cannot sign in, admin locked out | Forgotten password | `scripts/reset-admin-password.sh` or `hfproxy admin reset-password` |
| "saved settings cannot be decrypted" at start | `SESSION_SECRET` changed | Put the old value in `HFPROXY_SESSION_SECRET_PREVIOUS` (see Rotating the session secret) |
| Many users get 429 on the login page | Shared login limits: 20 attempts a minute per address, 10 per address and account, 50 failures a minute per account | Wait a minute; behind a proxy, set `HFPROXY_TRUSTED_PROXIES` so clients are told apart |

## Service levels

Suggested starting points, to adjust to your traffic:

- **Availability**: 99.5% of non-5xx responses on download routes over 30 days.
- **Latency**: 95% of cache hits start streaming within 500 ms.
- **Freshness**: a ref is confirmed with the Hub at least every `HFPROXY_CACHE_REF_TTL` while the Hub is reachable.

[prometheus-alerts.yml](prometheus-alerts.yml) contains matching alert rules, and [grafana-observability.json](grafana-observability.json) the dashboard.

## Running several replicas

Several instances can share one PostgreSQL database and one bucket. What is shared and what is per instance:

| State | Scope | Consequence |
| --- | --- | --- |
| Catalog, pins, settings, sessions, tokens | PostgreSQL, shared | Consistent everywhere |
| Login rate limits | PostgreSQL, shared | Limits hold across replicas |
| Cache fill of one file | A PostgreSQL advisory lock per file, shared | A file with a verifiable digest (LFS) is fetched from the Hub once; the other replica waits, then serves the cached copy |
| Concurrent requests for a file without a digest (small non-LFS files) | Per instance | Followers stream straight from the Hub instead of waiting, so a burst can fetch such a file several times |
| Negative cache (404/410 from the Hub), metadata cache, circuit breaker | Per instance | Each replica learns on its own; a replica may ask the Hub once more |
| Fill slots (`HFPROXY_CACHE_FILL_CONCURRENCY`), staging space | Per instance | The limits apply to each replica, so the fleet total is the limit times the number of replicas |
| Settings saved from the console | PostgreSQL, but loaded at start | Other replicas pick them up when they restart |

Pin one replica as the only one with `HFPROXY_AUTO_MIGRATE=true` (or run `hfproxy migrate up` as a job), and give every replica the same `HFPROXY_SESSION_SECRET`. A single replica remains a valid choice: failover is then a restart, and the cache survives because it lives in the bucket.

## Capacity

- Staging volume: `HFPROXY_CACHE_FILL_CONCURRENCY` times your largest file, plus headroom.
- Object storage: your hot set; set a quota and watch `hfproxy_cache_size_bytes`.
- PostgreSQL: small. `downloads` and `audit_events` are purged by the retention settings.
- CPU and memory: a cache hit is a streamed copy from object storage, so one or two vCPUs and 512 MiB to 1 GiB of memory serve a typical team. Expect the single-pass SHA-256 of a fill to use about one core per concurrent fill. Memory does not grow with file size, since files are streamed; it grows with `HFPROXY_API_CACHE_MAX_BYTES` (default 32 MiB) and the number of open connections.
- Bandwidth: a cold fill moves each file twice (Hub to the proxy, then to the bucket), a hit moves it once to the client. Size the proxy-to-bucket link for your largest burst of simultaneous downloads. Start from these figures and measure with your own models.
