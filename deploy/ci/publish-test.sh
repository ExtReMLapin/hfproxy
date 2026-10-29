#!/usr/bin/env bash
# Publishes to a running hfproxy with the real huggingface_hub client.
#
# Needs: Postgres on 127.0.0.1:5432 (user and password "postgres"), the
# `hfproxy` binary in $HFPROXY_BIN, and a Python with `moto[server]`,
# `huggingface_hub` and `requests`. $PY selects that Python.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
bin="${HFPROXY_BIN:-$root/hfproxy}"
py="${PY:-python3}"
work="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT

export PGPASSWORD=postgres
psql -q -h 127.0.0.1 -U postgres -c "CREATE DATABASE pubtest" >/dev/null

"$py" -m moto.server -H 127.0.0.1 -p 5055 >"$work/moto.log" 2>&1 &
for _ in $(seq 1 40); do curl -s -o /dev/null http://127.0.0.1:5055/ && break; sleep 0.5; done
curl -s -X PUT http://127.0.0.1:5055/hfproxy -o /dev/null

export HFPROXY_LISTEN_ADDR=127.0.0.1:18081 HFPROXY_PUBLIC_URL=http://127.0.0.1:18081
export HFPROXY_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/pubtest?sslmode=disable' HFPROXY_AUTO_MIGRATE=true
export HFPROXY_HF_BASE_URL=http://127.0.0.1:9 HFPROXY_HF_ALLOWED_REDIRECT_HOSTS=127.0.0.1
export HFPROXY_S3_ENDPOINT=http://127.0.0.1:5055 HFPROXY_S3_REGION=us-east-1 HFPROXY_S3_BUCKET=hfproxy
export HFPROXY_S3_ACCESS_KEY=test HFPROXY_S3_SECRET_KEY=test HFPROXY_S3_FORCE_PATH_STYLE=true
export HFPROXY_S3_SERVER_SIDE_ENCRYPTION=false HFPROXY_S3_PREFIX=pubtest
export HFPROXY_SESSION_SECRET=publish-test-secret-0123456789abcdef0123456789abcdef
export HFPROXY_STAGING_DIR="$work/staging" HFPROXY_LOG_LEVEL=info

"$bin" serve >"$work/serve.log" 2>&1 &
for _ in $(seq 1 60); do curl -s http://127.0.0.1:18081/readyz | grep -q '"ready"' && break; sleep 0.5; done

echo 'correct-horse-battery-staple' >"$work/pw"
"$bin" admin create-user --username alice --password-file "$work/pw" >/dev/null
psql -q -h 127.0.0.1 -U postgres pubtest <<'SQL'
INSERT INTO users(id,username,role) VALUES ('u-bob','bob','reader'),('u-carol','carol','reader'),('u-dave','dave','reader');
INSERT INTO groups(id,name) VALUES ('g-research','research');
INSERT INTO group_members(group_id,user_id) VALUES ('g-research','u-bob'),('g-research','u-carol');
SQL

token() { "$bin" admin token create --username "$1" --name "ci-$1-$2" --scope "$2" | awk '/^Token/{print $4}'; }
for u in alice bob carol dave; do
  export "TOKEN_$(echo "$u" | tr a-z A-Z)=$(token "$u" publish)"
done
export TOKEN_DAVE_READ="$(token dave read)"
export HF_ENDPOINT=http://127.0.0.1:18081
# The proxy does not speak Xet; clients fall back to Git LFS.
export HF_HUB_DISABLE_XET=1

if ! "$py" "$root/scripts/hub-client-test.py"; then
  tail -60 "$work/serve.log"
  exit 1
fi
