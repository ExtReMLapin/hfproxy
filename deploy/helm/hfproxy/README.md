# hfproxy Helm chart

Reference deployment: a `Deployment` running as UID 65532 with a read-only root filesystem, a `Service`, an optional TLS `Ingress`, a staging volume, probes on `/healthz` and `/readyz`, a `PodDisruptionBudget` (with several replicas), an optional `ServiceMonitor` and `PrometheusRule`, and a Helm hook `Job` that runs `hfproxy migrate up` before each install or upgrade.

## Install

1. Create the Secret. It holds files, never plain environment variables:

       kubectl create secret generic hfproxy \
         --from-file=database_url=./database_url \
         --from-file=session_secret=./session_secret \
         --from-file=s3_access_key=./s3_access_key \
         --from-file=s3_secret_key=./s3_secret_key

   `database_url` should use `sslmode=verify-full`. `session_secret` must hold at least 32 bytes; keep a copy with your other secrets (without it saved settings cannot be decrypted).

2. Install:

       helm install hfproxy ./deploy/helm/hfproxy \
         --set secrets.existingSecret=hfproxy \
         --set config.PUBLIC_URL=https://hub.example.com \
         --set config.S3_ENDPOINT=https://s3.example.com \
         --set config.S3_REGION=eu-west-1 \
         --set config.S3_BUCKET=hfproxy \
         --set ingress.enabled=true --set ingress.host=hub.example.com --set ingress.tlsSecretName=hub-tls

3. Create the first administrator (see the notes printed by Helm).

## Notes

- Set `config.TRUSTED_PROXIES` to the network of your ingress controller so login limits see client addresses.
- Size `staging.sizeLimit` for `CACHE_FILL_CONCURRENCY` times your largest file. `/readyz` fails while less than `STAGING_MIN_FREE_BYTES` is free.
- With `replicaCount > 1`, read "Running several replicas" in `docs/operations.md`.
- `files/prometheus-alerts.yml` is a copy of `docs/prometheus-alerts.yml`; CI checks they match.
- Pin the image by digest in production (`image.digest`).
