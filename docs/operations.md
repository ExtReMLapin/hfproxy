# Operations runbook

## Health and readiness

- `/healthz` answers while the process runs. `hfproxy healthcheck` probes it and is the container health check.
- `/readyz` answers 200 only when PostgreSQL is reachable, the schema is at the version this build expects, and the object-storage bucket is reachable. Route traffic on `/readyz`.

## Upgrades and rollbacks

1. Back up PostgreSQL (see below).
2. Run `hfproxy migrate status`, then `hfproxy migrate up`, or start one instance with `HFPROXY_AUTO_MIGRATE=true`.
3. Roll out the new image. A new build refuses to start until the schema is at its version, and `/readyz` stays unavailable until then.

Migrations only add tables, columns and indexes while a release depends on the old shape, so the previous image keeps working after a migration. To roll back, redeploy the previous image; do not remove migration rows by hand.

## Rotating the session secret

`HFPROXY_SESSION_SECRET` is the root of three separate keys: one hashes access tokens and sessions, one encrypts the saved settings, and one only names the key. To change it without logging everyone out or losing the saved settings:

1. Generate a new secret (`openssl rand -base64 48`) and store it with your other secrets.
2. On every instance set `HFPROXY_SESSION_SECRET` to the new value and `HFPROXY_SESSION_SECRET_PREVIOUS` to the old one (up to four old values, separated by commas or newlines; `HFPROXY_SESSION_SECRET_PREVIOUS_FILE` works too). Roll the instances out. Existing tokens, sessions and saved settings keep working; new ones use the new secret.
3. Run `hfproxy admin rotate-secret` once. It re-encrypts the saved settings under the new secret and prints how many active tokens and sessions are still stored under an older secret.
4. An access token moves to the new secret the first time it is used. Sessions end on their own. Revoke tokens nobody uses any more with `hfproxy admin token revoke`, or wait until `rotate-secret` reports 0.
5. When both counts are 0, remove `HFPROXY_SESSION_SECRET_PREVIOUS` and roll out again. From then on the old secret is useless.

If you forget step 2 and only replace `HFPROXY_SESSION_SECRET`, hfproxy refuses to start because the saved settings cannot be decrypted; put the old value in `HFPROXY_SESSION_SECRET_PREVIOUS` to recover. Tokens and sessions hashed with the old secret are refused until you do.

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
| Clients get 502 with `Upstream unavailable` | Hub outage or rate limiting | Cached files still serve; check `hfproxy_upstream_errors_total`; wait or use `HFPROXY_HF_PROXY_URL` |
| Clients get 503 `Cache fill capacity exhausted` | More simultaneous fills than `HFPROXY_CACHE_FILL_CONCURRENCY` | Retry; raise the limit if disk and bandwidth allow |
| Clients get 507 | Staging volume too small for the file | Enlarge `HFPROXY_STAGING_DIR` or lower concurrency |
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

## Capacity

- Staging volume: `HFPROXY_CACHE_FILL_CONCURRENCY` times your largest file, plus headroom.
- Object storage: your hot set; set a quota and watch `hfproxy_cache_size_bytes`.
- PostgreSQL: small. `downloads` and `audit_events` are purged by the retention settings.
