#!/usr/bin/env bash
# Installs the Helm chart in a kind cluster and checks that the service really
# works there: migrations, readiness, first-start setup, a cached download, a
# rolling upgrade to two replicas, and a replica killed in the middle of use.
#
# Needs: kind, kubectl, helm, docker, curl, python3. The image to test is
# loaded from the local Docker daemon (IMAGE, default hfproxy:ci).
set -euo pipefail

IMAGE="${IMAGE:-hfproxy:ci}"
CLUSTER="${CLUSTER:-hfproxy-ci}"
KIND="${KIND:-kind}"
HELM="${HELM:-helm}"
HERE="$(cd "$(dirname "$0")" && pwd)"
CHART="$HERE/../helm/hfproxy"
PORT=18080
HUB_PORT=18081
declare -A forwards

log() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAILED: $*" >&2; exit 1; }

diagnostics() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "--- cluster state at failure"
    kubectl get pods -o wide || true
    for pod in $(kubectl get pods -o name 2>/dev/null); do
      echo "--- logs of $pod"; kubectl logs "$pod" --all-containers --tail=40 || true
      echo "--- previous logs of $pod"; kubectl logs "$pod" --all-containers --previous --tail=40 2>/dev/null || true
    done
    kubectl describe pods -l app.kubernetes.io/name=hfproxy | tail -60 || true
    kubectl logs -l app.kubernetes.io/name=hfproxy --tail=80 --all-containers --prefix || true
    kubectl logs job/hfproxy-migrate --tail=40 || true
    kubectl get events --sort-by=.lastTimestamp | tail -25 || true
  fi
  for pid in "${forwards[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done
  if [ -z "${KEEP_CLUSTER:-}" ]; then "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  exit "$status"
}
trap diagnostics EXIT

# (Re)starts a port-forward to a service. A forward is tied to the pod it
# found at the start, so it is restarted after pods are replaced.
forward() { # service local-port remote-port
  if [ -n "${forwards[$1]:-}" ]; then kill "${forwards[$1]}" 2>/dev/null || true; fi
  kubectl port-forward "svc/$1" "$2:$3" >/dev/null 2>&1 &
  forwards[$1]=$!
}

wait_http() { # url expected-status [tries]
  local url="$1" want="$2" tries="${3:-60}" got=""
  for _ in $(seq "$tries"); do
    got="$(curl -s -o /dev/null -w '%{http_code}' "$url" || true)"
    [ "$got" = "$want" ] && return 0
    sleep 2
  done
  fail "$url answered $got, wanted $want"
}

log "create the cluster"
"$KIND" create cluster --name "$CLUSTER" --wait 120s
kind_image_load() { "$KIND" load docker-image "$IMAGE" --name "$CLUSTER"; }
kind_image_load

log "start PostgreSQL, S3 and the Hub stand-in"
# A self-signed certificate for the Hub stand-in; the chart trusts it as a CA.
CERTS="$(mktemp -d)"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=fakehub" -addext "subjectAltName=DNS:fakehub" \
  -keyout "$CERTS/tls.key" -out "$CERTS/tls.crt" 2>/dev/null
kubectl create secret generic fakehub-tls --from-file=tls.crt="$CERTS/tls.crt" --from-file=tls.key="$CERTS/tls.key"
kubectl create secret generic fakehub-ca --from-file=ca.crt="$CERTS/tls.crt"
kubectl create configmap fakehub-script --from-file=fakehub.py="$HERE/fakehub.py"
kubectl apply -f "$HERE/dependencies.yaml"
kubectl rollout status deploy/postgres --timeout=180s
kubectl rollout status deploy/moto --timeout=180s
kubectl rollout status deploy/fakehub --timeout=180s
forward moto 15000 5000
sleep 2
wait_http http://127.0.0.1:15000/moto-api/ 200 30
curl -sf -X PUT http://127.0.0.1:15000/hfproxy >/dev/null || fail "could not create the bucket"

REPO="${IMAGE%%:*}"
TAG="${IMAGE##*:}"
values=(
  --set image.repository="$REPO" --set image.tag="$TAG" --set image.pullPolicy=Never
  --set secrets.existingSecret=hfproxy --set secrets.files.SETUP_TOKEN=setup_token
  --set config.PUBLIC_URL=http://localhost:8080
  --set config.HF_BASE_URL=https://fakehub:8080 --set trustedCA.secretName=fakehub-ca
  --set config.HF_ALLOWED_PRIVATE_CIDRS=10.96.0.0/16 --set config.HF_ALLOWED_REDIRECT_HOSTS=fakehub
  --set config.S3_ENDPOINT=http://moto:5000 --set config.S3_REGION=us-east-1 --set config.S3_BUCKET=hfproxy
  --set config.S3_FORCE_PATH_STYLE=true --set config.S3_SERVER_SIDE_ENCRYPTION=false
  --set config.ANONYMOUS_PUBLIC_READ=true --set config.LOG_LEVEL=debug
  --set resources.limits.memory=512Mi
)

log "helm install (the migration runs as a hook)"
"$HELM" install hfproxy "$CHART" "${values[@]}" --wait --timeout 5m
kubectl get pods
kubectl logs job/hfproxy-migrate 2>/dev/null | tail -3 || true

forward hfproxy $PORT 8080
forward fakehub $HUB_PORT 8080
sleep 2
BASE="http://127.0.0.1:$PORT"

log "probes"
wait_http "$BASE/healthz" 200
wait_http "$BASE/readyz" 200
# Metrics live on their own port; the public port must not expose them. The
# console answers unknown paths with its page, so look at the body, not only
# at the status code.
curl -s "$BASE/metrics" | grep -Eq '^# (HELP|TYPE) ' && fail "/metrics on the public port serves Prometheus metrics to an anonymous caller"
code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/metrics")"
[ "$code" != 200 ] || echo "(the public port answers /metrics with the console page, not metrics)"

log "first start: the setup page is offered and the configured code works"
[ "$(curl -s "$BASE/api/v1/setup/status")" = '{"required":true}' ] || fail "setup should be required on a new instance"
code="$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{"setup_code":"wrong-code-wrong-code","username":"root","password":"a-long-enough-password"}' "$BASE/api/v1/setup")"
[ "$code" = 403 ] || fail "a wrong setup code answered $code"
code="$(curl -s -o /dev/null -w '%{http_code}' -c /tmp/kind-cookies -H 'Content-Type: application/json' -d '{"setup_code":"ci-setup-token-0123456789abcdef","username":"root","password":"a-long-enough-password"}' "$BASE/api/v1/setup")"
[ "$code" = 201 ] || fail "setup answered $code"
[ "$(curl -s "$BASE/api/v1/setup/status")" = '{"required":false}' ] || fail "setup should be closed"
[ "$(curl -s -b /tmp/kind-cookies "$BASE/api/v1/me" | python3 -c 'import json,sys;print(json.load(sys.stdin)["role"])')" = admin ] || fail "the new administrator is not signed in"

log "register a public repository"
csrf="$(awk '$6=="hfproxy_csrf"{print $7}' /tmp/kind-cookies)"
code="$(curl -s -o /dev/null -w '%{http_code}' -b /tmp/kind-cookies -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' -d '{"repo_type":"models","repo_id":"org/model","access_policy":"public"}' "$BASE/api/v1/repos")"
[ "$code" = 201 ] || [ "$code" = 200 ] || fail "adding the repository answered $code"

hub_hits() { curl -sk "https://127.0.0.1:$HUB_PORT/_hits"; }

download() { # path expected-status
  curl -s -o /tmp/kind-body -w '%{http_code}' "$BASE/org/model/resolve/main/$1"
}

# A small file is streamed to the first client while it is filled; the cache
# entry is published a moment after that response ends, and a read in between
# is deliberately served straight from the Hub. So wait for the entry.
wait_cached() { # path
  for _ in $(seq 1 40); do
    [ "$(curl -s -o /dev/null -D - "$BASE/org/model/resolve/main/$1" | tr -d '\r' | awk 'tolower($1)=="x-cache:"{print $2}')" = HIT ] && return 0
    sleep 0.5
  done
  fail "$1 never became a cache hit"
}

log "a small file is fetched from the Hub, then served from the cache"
[ "$(download config.json)" = 200 ] || fail "config.json (miss)"
grep -q bert /tmp/kind-body || fail "config.json has the wrong content"
wait_cached config.json
small_hits="$(hub_hits)"
[ "$(download config.json)" = 200 ] || fail "config.json (hit)"
[ "$(download config.json)" = 200 ] || fail "config.json (second hit)"
grep -q bert /tmp/kind-body || fail "cached config.json has the wrong content"
[ "$(hub_hits)" = "$small_hits" ] || fail "reads of a cached config.json reached the Hub ($small_hits -> $(hub_hits))"

log "a large file behind a redirect keeps its content, and is cached too"
expected="$(python3 -c "import hashlib;print(hashlib.sha256(b'weights-'*131072).hexdigest())")"
[ "$(download model.bin)" = 200 ] || fail "model.bin (miss)"
[ "$(sha256sum /tmp/kind-body | cut -d' ' -f1)" = "$expected" ] || fail "model.bin changed on the way"
hub_hits_after_first="$(hub_hits)"
[ "$(download model.bin)" = 200 ] || fail "model.bin (hit)"
[ "$(sha256sum /tmp/kind-body | cut -d' ' -f1)" = "$expected" ] || fail "cached model.bin changed"
[ "$(hub_hits)" = "$hub_hits_after_first" ] || fail "the second read of model.bin reached the Hub"
code="$(curl -s -o /tmp/kind-body -w '%{http_code}' -H 'Range: bytes=8-15' "$BASE/org/model/resolve/main/model.bin")"
[ "$code" = 206 ] && [ "$(cat /tmp/kind-body)" = "weights-" ] || fail "range request answered $code"

log "rolling update to two replicas keeps serving"
"$HELM" upgrade hfproxy "$CHART" "${values[@]}" --set replicaCount=2 --wait --timeout 5m
kubectl get pods -l app.kubernetes.io/name=hfproxy
[ "$(kubectl get pods -l app.kubernetes.io/name=hfproxy --field-selector=status.phase=Running -o name | wc -l)" -ge 2 ] || fail "expected two running replicas"
forward hfproxy $PORT 8080
sleep 3
wait_http "$BASE/readyz" 200
[ "$(download config.json)" = 200 ] || fail "config.json after the upgrade"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/setup/status")" = 200 ]

log "a replica is killed: the other one keeps answering, and a new one replaces it"
victim="$(kubectl get pods -l app.kubernetes.io/name=hfproxy -o name | head -1)"
kubectl delete "$victim" --wait=false
for _ in $(seq 20); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz" || true)" = 200 ] && break
  sleep 1
done
kubectl rollout status deploy/hfproxy --timeout=180s
forward hfproxy $PORT 8080
sleep 3
wait_http "$BASE/readyz" 200
[ "$(download model.bin)" = 200 ] || fail "model.bin after a replica was killed"
[ "$(sha256sum /tmp/kind-body | cut -d' ' -f1)" = "$expected" ] || fail "model.bin changed after a replica was killed"

log "OK: the chart works in a real cluster"
