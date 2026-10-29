#!/usr/bin/env bash
# Starts a throwaway Keycloak with one realm for the OIDC integration tests and
# prints the environment the tests read. Usage:
#   eval "$(deploy/ci/keycloak-up.sh)"
# Needs docker, curl and python3. Stop it with: docker rm -f hfproxy-keycloak
set -euo pipefail
IMAGE="${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:26.1.4}"
PORT="${KEYCLOAK_PORT:-8180}"
NAME=hfproxy-keycloak
CLIENT_SECRET="keycloak-test-client-secret"

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p "127.0.0.1:$PORT:8080" \
  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
  "$IMAGE" start-dev >/dev/null

BASE="http://localhost:$PORT"
for _ in $(seq 1 120); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/realms/master" || true)" = 200 ] && break
  sleep 2
done
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/realms/master" || true)" = 200 ] || { docker logs "$NAME" >&2; echo "Keycloak did not start" >&2; exit 1; }

TOKEN="$(curl -sf -d client_id=admin-cli -d username=admin -d password=admin -d grant_type=password \
  "$BASE/realms/master/protocol/openid-connect/token" | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')"

python3 - "$CLIENT_SECRET" >/tmp/hfproxy-realm.json <<'PYEOF'
import json, sys
secret = sys.argv[1]
def user(name, email, verified, first, last):
    return {"username": name, "email": email, "emailVerified": verified, "enabled": True,
            "firstName": first, "lastName": last,
            "credentials": [{"type": "password", "value": name + "-password", "temporary": False}]}
print(json.dumps({
    "realm": "hfproxy", "enabled": True,
    "users": [
        user("dana", "dana@example.com", True, "Dana", "Smith"),
        user("erin", "erin@example.com", False, "Erin", "Jones"),
    ],
    "clients": [{
        "clientId": "hfproxy", "enabled": True, "protocol": "openid-connect",
        "publicClient": False, "secret": secret, "standardFlowEnabled": True,
        "directAccessGrantsEnabled": False,
        "redirectUris": ["http://localhost:8080/auth/callback"],
    }],
}))
PYEOF
code="$(curl -s -o /tmp/hfproxy-realm.out -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data @/tmp/hfproxy-realm.json "$BASE/admin/realms")"
[ "$code" = 201 ] || { cat /tmp/hfproxy-realm.out >&2; echo "creating the realm answered $code" >&2; exit 1; }

cat <<ENV
export HFPROXY_TEST_OIDC_ISSUER=$BASE/realms/hfproxy
export HFPROXY_TEST_OIDC_CLIENT_ID=hfproxy
export HFPROXY_TEST_OIDC_CLIENT_SECRET=$CLIENT_SECRET
ENV
