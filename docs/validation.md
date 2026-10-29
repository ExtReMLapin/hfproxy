# Validating a deployment

What the automated tests cannot tell you, because it needs your cluster, your object store and the real Hugging Face Hub. Do these before relying on a deployment; each ends with what to look at and when to stop.

CI already covers a lot: the chart is installed in a throwaway (kind) cluster and exercised against a stand-in Hub, a replica is killed during a cache fill, the LDAP and OIDC sign-ins run against a real OpenLDAP and a real Keycloak, and a 2 GiB file goes through the real binary. That is not your environment: different storage, network, ingress, policies and a Hub that rate limits.

## 1. Install the chart on a real cluster

```bash
helm install hfproxy oci://ghcr.io/hfproxy/charts/hfproxy --version X.Y.Z -f values.yaml   # or ./deploy/helm/hfproxy
kubectl get pods,jobs,pdb
kubectl logs job/hfproxy-migrate
```

Look at, in this order:
- the migration job completes, then the pods become ready (`/readyz`); a pod that stays unready tells you why in its probe output;
- the first-start setup page works and the setup code in the logs is accepted ([First start](operations.md#first-start));
- the ingress serves HTTPS on the public URL, the console loads without CSP errors in the browser console, and `/metrics` is **not** reachable on the public address;
- Prometheus scrapes the pods (`up{job="hfproxy"} == 1`) and the alert rules load;
- `kubectl rollout restart deploy/hfproxy` and a `helm upgrade` complete without failed requests (run a download loop beside them), and the PodDisruptionBudget lets a node drain proceed.

Stop and fix before going on if a pod cannot reach PostgreSQL or the bucket (the log names which), or if readiness flaps.

## 2. A large model through the real Hub

Register a repository whose files are large (a 10 GB+ model; the real thing you intend to serve is best), then from a client machine:

```bash
export HF_ENDPOINT=https://hfproxy.example.com   # your public URL
export HF_TOKEN=<an hfproxy token>               # not needed for a public repository
pip install -U huggingface_hub
time huggingface-cli download ORG/MODEL --local-dir /tmp/model-cold
time huggingface-cli download ORG/MODEL --local-dir /tmp/model-warm
```

Record, for the cold and the warm download:

| Measure | Where |
| --- | --- |
| Duration, average speed | `time` above |
| Files identical to the Hub's | `sha256sum` of the files against the same download made directly from huggingface.co (`HF_ENDPOINT` unset) |
| Proxy memory, peak | `kubectl top pod` during the transfer, or `container_memory_working_set_bytes` |
| Staging use, peak | `hfproxy_staging_free_bytes` and `hfproxy_staging_reserved_bytes` |
| Hub errors, retries, circuit | `hfproxy_upstream_errors_total`, `hfproxy_upstream_retries_total`, `hfproxy_upstream_breaker_opened_total` |
| Cache hit on the second pass | `X-Cache: HIT` (`curl -sI`), `hfproxy_cache_requests_total{result="hit"}` |

Then the awkward cases:
- **Interrupt and resume.** Cancel the cold download halfway (Ctrl-C), run it again: it must finish with the same digests, and the Hub should be asked only for what is missing (the fill resumes from the last stored block).
- **Two clients, same cold file.** Start both together: the file is fetched from the Hub once.
- **A Hub redirect host you did not allow.** The logs name it ("upstream request error", with the host and cause); add the delivery hosts you see to `HFPROXY_HF_ALLOWED_REDIRECT_HOSTS` and repeat. The Hub's CDN hostnames change over time, so keep the list under review.
- **Rate limiting.** If the Hub answers 429 under load, set `HFPROXY_HF_TOKEN` and lower `HFPROXY_CACHE_FILL_CONCURRENCY`.
- `hfproxy cache verify` afterwards re-reads everything and must report 0 invalid.

Stop if the digests differ from the direct download, if memory grows with the file size, or if the staging volume cannot hold your largest file times the fill concurrency ([sizing.md](sizing.md)).

## 3. Backup and restore drill

`scripts/backup-restore-drill.sh` rehearses a restore on a **copy** (a scratch database and bucket; production is only read), starts a second hfproxy on the copy, checks readiness, runs `hfproxy cache verify` and downloads a file, expecting a cache hit. Read the header of the script for the variables, then:

```bash
SOURCE_DATABASE_URL=… SOURCE_BUCKET=… ADMIN_DATABASE_URL=… \
HFPROXY_SESSION_SECRET=… HFPROXY_S3_ENDPOINT=… HFPROXY_S3_ACCESS_KEY=… HFPROXY_S3_SECRET_KEY=… \
CHECK_PATH=org/model/resolve/main/config.json CHECK_TOKEN=… \
  scripts/backup-restore-drill.sh
```

It prints the time of each step and ends with `OK` or `FAILED`. The part of your recovery time that belongs to hfproxy is "restore + bucket copy + ready": compare it with what you promise your users. Repeat it after each major upgrade, and whenever the database or bucket grows by an order of magnitude. A restore you have never tried is not a backup.

## 4. A multi-gigabyte file on your own machine

`scripts/large-file-test.py` starts the real binary against your PostgreSQL and S3, serves a generated file of the size you choose from a stand-in Hub, and prints the proxy's peak memory and staging use for a cold fill, a cached read and a range read:

```bash
scripts/large-file-test.py --binary ./hfproxy --size-gib 10 --max-rss-mib 600
```

Run it against the same kind of object store you use in production (not `moto`, which keeps everything in RAM) to see the real throughput of fill and read.

## What to send back

If something fails, keep: the output of the step, the pod or container logs around it (`kubectl logs … --since=30m`, with the log level at `info`), the `request_id` of an affected response, and the metrics named above. Remove tokens and passwords first.
