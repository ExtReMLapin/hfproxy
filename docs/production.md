# Running hfproxy in production

The provided `docker-compose.yml` is for a trusted development machine. It uses published demo passwords and plain HTTP. Do not reuse it as is; start from `docker-compose.example.yml` and `Caddyfile.example`.

## Startup checks

`hfproxy serve` refuses to start when:

- `HFPROXY_SESSION_SECRET` is the published development value and `HFPROXY_PUBLIC_URL` is not a loopback address;
- `HFPROXY_PUBLIC_URL` is empty while listening on a non-loopback address.

When `HFPROXY_PUBLIC_URL` is `https://…`, session cookies are `Secure` and responses carry `Strict-Transport-Security`.

## Checklist

1. **TLS.** hfproxy speaks plain HTTP. Terminate TLS in a reverse proxy, set `HFPROXY_PUBLIC_URL` to the public `https://` URL, and list the proxy addresses in `HFPROXY_TRUSTED_PROXIES` so login rate limiting sees client addresses.
2. **Secrets.** Provide `SESSION_SECRET`, `DATABASE_URL`, `S3_ACCESS_KEY` and `S3_SECRET_KEY` with the `HFPROXY_*_FILE` variants from a secrets manager. Generate the session secret with `openssl rand -base64 48`. Keep a backup: it protects the saved settings, access tokens and sessions. To change it without breaking them, follow [Rotating the session secret](operations.md#rotating-the-session-secret).
3. **PostgreSQL.** Use `sslmode=require` (or stricter) and a dedicated role. Run `hfproxy migrate up` before starting a new version, or set `HFPROXY_AUTO_MIGRATE=true` on one instance.
4. **Object storage.** Keep the bucket private. Plain-HTTP endpoints are supported on a trusted network; prefer HTTPS otherwise.
5. **Disk.** Mount a volume for `HFPROXY_STAGING_DIR` large enough for `HFPROXY_CACHE_FILL_CONCURRENCY` times your largest file, and alert on `hfproxy_staging_free_bytes`. With several instances behind a load balancer, set `HFPROXY_STAGING_MIN_FREE_BYTES` (for example to your largest file) so `/readyz` takes an instance with a full staging disk out of rotation.
6. **Capacity.** Set `HFPROXY_CACHE_MAX_TOTAL_SIZE` and alert on `hfproxy_cache_size_bytes` and `hfproxy_cache_evictions_total`. Pin (`hfproxy cache pin`) the repositories, commits or branches that must always stay cached, such as the models production depends on (pin a commit SHA for an exact version, or a branch to keep whatever it points to now); keep `hfproxy_cache_pinned_bytes` well under the quota, since pinned files are never evicted.
7. **Metrics.** Set `HFPROXY_METRICS_ADDR` to an internal interface and scrape that port; do not publish `/metrics` through the reverse proxy.
8. **Content origin.** Repository files are untrusted. hfproxy sends `Content-Security-Policy: sandbox` and forces HTML, SVG, XML and script files to download, but for defense in depth serve the proxy's file routes from a different hostname than the administration console.
9. **Several instances.** Login throttling is shared through PostgreSQL. The negative cache and the single-flight table that deduplicates cache fills are still per process: replicas stay correct, but may fetch the same file from the Hub at the same time.
10. **Tokens.** Give automation `read` tokens. A `read` token cannot use the administration API, even when its owner is an administrator.

## Backups

Back up PostgreSQL and the S3 bucket together and restore them from the same point in time: the catalog references objects by hash. Also back up `SESSION_SECRET`.

## Hub failures

- **`HFPROXY_CACHE_REF_TTL` (default 1 minute).** This is how long a branch or tag is trusted before hfproxy asks the Hub again. It is deliberately short so a moving branch such as `main` is picked up quickly. A longer value means fewer Hub requests and slower updates; it no longer decides whether files keep working during an outage, because a ref that cannot be confirmed falls back to the last known commit. Raise it only if you are close to the Hub's rate limits.
- **Circuit breaker.** After five consecutive failures of a Hub host, requests to it fail immediately for 30 seconds (and cached files are served), then one probe decides whether to resume. Watch `hfproxy_upstream_breaker_opened_total`.
- **Metadata cache.** Repository, revision and tree responses are cached in memory and served stale for up to 24 hours when the Hub is failing. Each process has its own cache.

## More

- [operations.md](operations.md): upgrades, rollbacks, incidents and service levels.
- [threat-model.md](threat-model.md): what hfproxy defends against.
- [prometheus-alerts.yml](prometheus-alerts.yml): example alert rules.
