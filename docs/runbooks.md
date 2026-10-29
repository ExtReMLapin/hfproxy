# Runbooks

One section per alert in [prometheus-alerts.yml](prometheus-alerts.yml); each alert's `runbook_url` points here. Every section says what the alert means, how to confirm it and what to do. [operations.md](operations.md#incidents) has the symptom table for problems that raise no alert.

Two things help with all of them. Every error response carries a `request_id` that is also in the logs, together with the cause the client is not told. Setting the log level to `debug` (Settings → Observability, no restart) adds the request path of each download; set it back afterwards.

## HfproxyDown

**Meaning.** Prometheus cannot scrape the metrics endpoint for 2 minutes. The process may be down, or only the metrics path may be unreachable.

**Check.**
- `curl -s http://<instance>:8080/healthz`: if it answers, the process is up and the problem is the scrape (network policy, `HFPROXY_METRICS_ADDR`, the ServiceMonitor, a changed `HFPROXY_METRICS_PATH`).
- Otherwise look at the container: `kubectl get pods`, `kubectl describe pod`, `kubectl logs --previous`. A start that fails on configuration prints the reason and exits.
- `/readyz` answering 503 means the process is up but cannot serve: see the reason in its body (database, bucket, `staging_space`, schema behind).

**Act.** Fix the cause shown above. A crash loop right after an upgrade: roll back (see [Upgrades and rollbacks](operations.md#upgrades-and-rollbacks)). If every replica is down at once, check PostgreSQL first, since migrations and the readiness probe both need it.

## HfproxyServerErrors

**Meaning.** More than 5% of requests failed with a 5xx for 10 minutes.

**Check.** Find which route and status:

```promql
topk(10, sum by (route, status_code) (rate(hfproxy_http_requests_total{status_code=~"5.."}[5m])))
```

| Status | Usual cause | Go to |
| --- | --- | --- |
| 502 on download routes | Hub unreachable or failing | [HfproxyUpstreamErrors](#hfproxyupstreamerrors) |
| 502 `The file could not be completed` | A cache fill was interrupted or the object store refused a write | [HfproxyStorageErrors](#hfproxystorageerrors); the client can retry and resumes |
| 503 `Cache fill capacity exhausted` | More simultaneous cold fills than `HFPROXY_CACHE_FILL_CONCURRENCY` | Raise it if disk and bandwidth allow, or accept that clients retry |
| 507 | Staging volume too small for the file | [HfproxyStagingSpaceLow](#hfproxystagingspacelow) |
| 500 | A bug or a failing dependency | Search the logs for the `request_id` of an affected response |

**Act.** Treat the cause, not the symptom. If one route fails for one repository only, check its access policy and whether the Hub still serves it.

## HfproxyUpstreamErrors

**Meaning.** Requests to Hugging Face fail at more than 0.1 per second for 15 minutes. Cached files keep being served; anything not yet cached fails.

**Check.**

```promql
sum by (reason) (rate(hfproxy_upstream_errors_total[10m]))
```

`timeout` and `connection` point to the network or the Hub; `http_4xx` and `http_5xx` to the Hub's answers (among the 4xx, 429 is rate limiting and 401/403 a bad or missing token). `other` is anything else: the log line **"upstream request error"** gives the host, the operation and the exact cause (the URL is left out on purpose).

- Cause "egress to non-public IP is blocked": the Hub URL or a redirect resolves to a private address. A mirror inside your network needs `HFPROXY_HF_ALLOWED_PRIVATE_CIDRS` (see [production.md](production.md#upstream-addresses)).
- TLS errors: a mirror signed by a private CA needs `trustedCA` in the chart, and a restart after changing it.
- Check [status.huggingface.co](https://status.huggingface.co) and, from the instance, whether `HFPROXY_HF_BASE_URL` answers.

**Act.** If it is the Hub, wait: the circuit breaker already makes requests fail fast. If it is your egress path, fix it or set `HFPROXY_HF_PROXY_URL`. For rate limiting, set `HFPROXY_HF_TOKEN` (authenticated limits are higher) and lower the fill concurrency.

## HfproxyUpstreamCircuitOpen

**Meaning.** After five consecutive failures the circuit for a Hub host opened: requests to it fail fast for the cooldown, then one probe tries again. Cached files are unaffected.

**Check.** The `host` label says which host. Log line "upstream circuit opened". Then follow [HfproxyUpstreamErrors](#hfproxyupstreamerrors).

**Act.** Nothing if the Hub is down: the circuit closes by itself after a successful probe. If it opens repeatedly while the Hub is healthy, the problem is on the path between you and it (proxy, DNS, firewall, MTU). `HFPROXY_UPSTREAM_BREAKER_THRESHOLD` and `HFPROXY_UPSTREAM_BREAKER_COOLDOWN` tune it; do not raise the threshold to hide a real problem.

## HfproxyUpstreamRetries

**Meaning.** hfproxy retries Hub requests more than 0.1 per second for 15 minutes: the Hub is rate limiting or unstable, but not yet failing outright.

**Check.** `sum by (operation) (rate(hfproxy_upstream_retries_total[10m]))` and the status classes in `hfproxy_upstream_requests_total`. A rise in `4xx` with retries is rate limiting.

**Act.** Configure `HFPROXY_HF_TOKEN`, lower `HFPROXY_CACHE_FILL_CONCURRENCY`, or preload the models you know you need during a quiet period (administrators can start a preload from the console).

## HfproxyStorageErrors

**Meaning.** Operations on the object store are failing. Cache hits cannot be served and fills cannot be stored.

**Check.**

```promql
sum by (operation) (rate(hfproxy_storage_errors_total[10m]))
```

Then the logs for the S3 error text. Typical causes: expired or rotated credentials (`AccessDenied`, `InvalidAccessKeyId`), a deleted or renamed bucket, an endpoint or certificate change, the store itself being down or full, a clock skew larger than 15 minutes (`RequestTimeTooSkewed`). `/readyz` also fails when the bucket is unreachable.

**Act.** Restore access to the bucket. After rotating S3 credentials, update the secret and roll the instances ([rotation.md](rotation.md#object-storage-credentials)). When storage returns, `hfproxy cache verify` rereads objects and finds anything left inconsistent.

## HfproxyGarbageCollectionFailing

**Meaning.** The periodic collection of orphaned blobs failed in the last three hours. Nothing is lost, but storage keeps growing.

**Check.** The log line "orphaned blob collection failed" and `hfproxy_gc_runs_total{result="error"}`. The usual cause is the same as [HfproxyStorageErrors](#hfproxystorageerrors), in particular missing permission to **delete** objects (the service identity needs read, write and delete).

**Act.** Fix the storage permission or the database connection, then wait for the next run (`hfproxy_gc_last_run_timestamp_seconds`) or restart one instance.

## HfproxyGarbageCollectionStalled

**Meaning.** The last collection on this instance was more than three hours ago, though collections have run before. Each instance runs one pass every hour, one pass at a time, so a pass that never returns blocks all the following ones.

**Check.** Whether the object store answers slowly or not at all: `histogram_quantile(0.95, sum by (le) (rate(hfproxy_storage_operation_duration_seconds_bucket[10m])))` and [HfproxyStorageErrors](#hfproxystorageerrors). A very large bucket listing or a store that accepts connections but does not answer are the usual reasons. The line "orphaned blob collection completed" (logged only when something was deleted) and the timestamp metric `hfproxy_gc_last_run_timestamp_seconds` show whether passes resume.

**Act.** Restart the affected instance: the loop starts again with the next hour. Fix the storage problem behind it, otherwise it will happen again.

## HfproxyCacheNearQuota

**Meaning.** The cache is above 90% of `HFPROXY_CACHE_MAX_TOTAL_SIZE` (the threshold in the rule is a number you set to match).

**Check.** `hfproxy_cache_size_bytes`, `hfproxy_cache_pinned_bytes` and the eviction rate (`hfproxy_cache_evictions_total`). Under the quota, eviction removes the least recently used unpinned files; the alert is only a warning that the working set is approaching the limit.

**Act.** Raise the quota and the bucket capacity if the working set is larger than planned, or accept eviction. Do not delete objects from the bucket by hand: use `hfproxy cache purge` (dry run by default).

## HfproxyStagingSpaceLow

**Meaning.** Free space on the staging volume is below the threshold you set (a little more than your largest file). New large fills are refused with 507 and the instance reports `staging_space` on `/readyz`.

**Check.** `hfproxy_staging_free_bytes` against `hfproxy_staging_reserved_bytes` (space promised to fills in progress), and `df` inside the container. An `emptyDir` staging volume is emptied when the pod is replaced; with a PVC, look in the staging directory for large files of interrupted fills.

**Act.** Enlarge the volume (with the chart: `staging.size` for a PVC, `staging.sizeLimit` for `emptyDir`), or lower `HFPROXY_CACHE_FILL_CONCURRENCY`. Size it as concurrency times your largest file, plus headroom ([sizing.md](sizing.md)).

## HfproxyPinnedContentNearQuota

**Meaning.** Pinned files take more than 80% of the quota, leaving little room for eviction to work with.

**Check.** `hfproxy cache pins` lists what is pinned.

**Act.** Unpin what is no longer essential, or raise `HFPROXY_CACHE_MAX_TOTAL_SIZE`. If pinned files alone exceed the quota, hfproxy logs "the cache is over its size limit but what remains is pinned" and cannot evict further.

## HfproxyHeavyEviction

**Meaning.** The cache has evicted more than one file per second on average for two hours. Information only: files are being fetched again because the quota is smaller than the working set.

**Check.** Compare the eviction rate with the miss rate: `sum(rate(hfproxy_cache_requests_total{result="miss"}[1h]))`. A high miss rate together with high upstream traffic confirms thrashing.

**Act.** Raise the quota if the extra storage is acceptable, or pin the files that must always be local.
