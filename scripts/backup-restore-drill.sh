#!/usr/bin/env bash
# Rehearses a restore on a COPY and tells you how long it took and whether it works.
#
# It dumps the production database, copies the bucket, restores both into a scratch
# database and a scratch bucket, starts a second hfproxy on the copy, checks that it
# becomes ready, runs `hfproxy cache verify` (re-reads every object and checks its
# SHA-256) and, if you give it a file, downloads that file and expects a cache hit.
# Production is only read. Run it from a machine that can reach both.
#
# Needs: pg_dump, pg_restore, psql, the AWS CLI (any S3-compatible store: set
# AWS_ENDPOINT_URL and the usual AWS_* credentials), curl, and the hfproxy binary.
#
#   SOURCE_DATABASE_URL   production database (read only)         postgres://user:pw@host/db
#   SOURCE_BUCKET         production bucket (read only)           hfproxy
#   ADMIN_DATABASE_URL    a database of the same server where the scratch database can be created
#   HFPROXY_SESSION_SECRET  the production session secret (saved settings cannot be read without it)
#   HFPROXY_S3_ENDPOINT, HFPROXY_S3_REGION, HFPROXY_S3_ACCESS_KEY, HFPROXY_S3_SECRET_KEY,
#   HFPROXY_S3_FORCE_PATH_STYLE   how the restored instance reaches the store (as in production)
# Optional:
#   SCRATCH_DB_NAME (hfproxy_restore_drill)   SCRATCH_BUCKET (hfproxy-restore-drill)
#   HFPROXY_BIN (./hfproxy)   DRILL_PORT (18490)
#   CHECK_PATH  a cached file to download, e.g. org/model/resolve/main/config.json
#   CHECK_TOKEN a token allowed to read it (not needed for a public repository)
#
# Usage: scripts/backup-restore-drill.sh [--keep]     (--keep leaves the copy in place)
set -euo pipefail

keep=false
[ "${1:-}" = "--keep" ] && keep=true

need() { [ -n "${!1:-}" ] || { echo "$1 is not set (see the top of this script)" >&2; exit 2; }; }
for v in SOURCE_DATABASE_URL SOURCE_BUCKET ADMIN_DATABASE_URL HFPROXY_SESSION_SECRET HFPROXY_S3_ENDPOINT; do need "$v"; done
for c in pg_dump pg_restore psql aws curl; do command -v "$c" >/dev/null || { echo "$c is required" >&2; exit 2; }; done
bin="${HFPROXY_BIN:-./hfproxy}"
[ -x "$bin" ] || { echo "hfproxy binary not found at $bin (set HFPROXY_BIN)" >&2; exit 2; }
scratch_db="${SCRATCH_DB_NAME:-hfproxy_restore_drill}"
scratch_bucket="${SCRATCH_BUCKET:-hfproxy-restore-drill}"
port="${DRILL_PORT:-18490}"

# Never let a mistake point the drill at production.
source_db="$(psql "$SOURCE_DATABASE_URL" -Atc 'select current_database()')"
if [ "$scratch_db" = "$source_db" ] || [ "$scratch_bucket" = "$SOURCE_BUCKET" ]; then
  echo "The scratch database and bucket must differ from the production ones." >&2
  exit 2
fi

work="$(mktemp -d)"
pid=""
cleanup() {
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  if ! $keep; then
    psql "$ADMIN_DATABASE_URL" -qc "DROP DATABASE IF EXISTS \"$scratch_db\"" >/dev/null 2>&1 || true
    aws s3 rb "s3://$scratch_bucket" --force >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
timer() { date +%s; }
declare -A took
counts() { psql "$1" -Atc "select 'repositories '||(select count(*) from repositories)||', artifacts '||(select count(*) from artifacts)||', blobs '||(select count(*) from blobs)||', users '||(select count(*) from users)"; }

step "1. Dump the production database"
t=$(timer)
pg_dump --format=custom --no-owner --file "$work/hfproxy.dump" "$SOURCE_DATABASE_URL"
took[dump]=$(( $(timer) - t ))
before="$(counts "$SOURCE_DATABASE_URL")"
echo "dump: $(du -h "$work/hfproxy.dump" | cut -f1) in ${took[dump]}s; production now has $before"

step "2. Copy the bucket"
t=$(timer)
aws s3 mb "s3://$scratch_bucket" >/dev/null
aws s3 sync "s3://$SOURCE_BUCKET" "s3://$scratch_bucket" --only-show-errors
took[bucket]=$(( $(timer) - t ))
echo "bucket copied in ${took[bucket]}s"

step "3. Restore the database into $scratch_db"
t=$(timer)
psql "$ADMIN_DATABASE_URL" -qc "DROP DATABASE IF EXISTS \"$scratch_db\"" -c "CREATE DATABASE \"$scratch_db\""
scratch_url="$(python3 - "$ADMIN_DATABASE_URL" "$scratch_db" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit
u = urlsplit(sys.argv[1])
print(urlunsplit((u.scheme, u.netloc, "/" + sys.argv[2], u.query, "")))
PY
)"
pg_restore --no-owner --dbname "$scratch_url" "$work/hfproxy.dump"
took[restore]=$(( $(timer) - t ))
after="$(counts "$scratch_url")"
echo "restored in ${took[restore]}s; the copy has $after"
if [ "$before" != "$after" ]; then
  echo "NOTE: the counts differ from production now. That is expected if production changed since the dump; it is a problem if it did not."
fi

step "4. Start a second hfproxy on the copy"
export HFPROXY_DATABASE_URL="$scratch_url" HFPROXY_S3_BUCKET="$scratch_bucket"
export HFPROXY_LISTEN_ADDR="127.0.0.1:$port" HFPROXY_PUBLIC_URL="http://127.0.0.1:$port" HFPROXY_AUTO_MIGRATE=false
export HFPROXY_METRICS_ENABLED=false HFPROXY_LOG_LEVEL=warn
t=$(timer)
"$bin" serve >"$work/serve.log" 2>&1 &
pid=$!
ready=false
for _ in $(seq 60); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/readyz" || true)" = 200 ]; then ready=true; break; fi
  kill -0 "$pid" 2>/dev/null || break
  sleep 1
done
took[ready]=$(( $(timer) - t ))
if ! $ready; then
  echo "The restored instance did not become ready. Its log:" >&2
  cat "$work/serve.log" >&2
  exit 1
fi
echo "ready after ${took[ready]}s"

step "5. Verify every cached object (re-reads and checks the SHA-256)"
t=$(timer)
verify_status=0
"$bin" cache verify | tee "$work/verify.log" | tail -n 20 || verify_status=$?
took[verify]=$(( $(timer) - t ))
echo "verified in ${took[verify]}s (exit status $verify_status)"

check_status="not requested"
if [ -n "${CHECK_PATH:-}" ]; then
  step "6. Download $CHECK_PATH from the copy"
  auth=()
  [ -n "${CHECK_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $CHECK_TOKEN")
  code="$(curl -s -o "$work/check.out" -D "$work/check.headers" -w '%{http_code}' "${auth[@]}" "http://127.0.0.1:$port/$CHECK_PATH" || true)"
  cache="$(tr -d '\r' < "$work/check.headers" | awk 'tolower($1)=="x-cache:"{print $2}')"
  check_status="HTTP $code, X-Cache ${cache:-none}"
  echo "$check_status"
fi

step "Summary"
printf 'dump %ss | bucket copy %ss | restore %ss | ready %ss | verify %ss\n' "${took[dump]}" "${took[bucket]}" "${took[restore]}" "${took[ready]}" "${took[verify]}"
echo "production: $before"
echo "copy:       $after"
echo "cache verify exit status: $verify_status"
echo "check download: $check_status"
echo
echo "Time to a working copy (restore + bucket copy + ready): $(( ${took[bucket]} + ${took[restore]} + ${took[ready]} ))s, the part of your recovery time that belongs to hfproxy."
if [ "$verify_status" != 0 ] || { [ -n "${CHECK_PATH:-}" ] && [[ "$check_status" != "HTTP 200, X-Cache HIT" ]]; }; then
  echo "FAILED: see above."
  exit 1
fi
echo "OK"
