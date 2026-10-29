#!/usr/bin/env bash
# Starts a throwaway OpenLDAP server for the directory integration tests and
# prints the environment the tests read. Usage:
#   eval "$(deploy/ci/ldap-up.sh /path/to/workdir)"
# Needs slapd and openssl. Stop it with: kill "$(cat WORKDIR/slapd.pid)"
set -euo pipefail
WORK="${1:?usage: ldap-up.sh WORKDIR}"
HERE="$(cd "$(dirname "$0")" && pwd)"
LDAPS_PORT="${LDAPS_PORT:-1636}"
LDAP_PORT="${LDAP_PORT:-1389}"
mkdir -p "$WORK/data"
rm -rf "$WORK/data"/*

# A private CA and a server certificate for localhost.
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=hfproxy-test-ca" \
  -keyout "$WORK/ca.key" -out "$WORK/ca.pem" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=localhost" -keyout "$WORK/server.key" -out "$WORK/server.csr" 2>/dev/null
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' >"$WORK/san.ext"
openssl x509 -req -in "$WORK/server.csr" -CA "$WORK/ca.pem" -CAkey "$WORK/ca.key" -CAcreateserial \
  -days 2 -extfile "$WORK/san.ext" -out "$WORK/server.pem" 2>/dev/null

# SSL_CERT_FILE replaces the system roots for the whole test process, so the
# bundle keeps them and adds the test CA.
SYSTEM_ROOTS=/etc/ssl/certs/ca-certificates.crt
{ [ -f "$SYSTEM_ROOTS" ] && cat "$SYSTEM_ROOTS"; cat "$WORK/ca.pem"; } >"$WORK/trust.pem"

SCHEMA=/etc/ldap/schema
MODULES=/usr/lib/ldap
[ -d "$MODULES" ] || MODULES=/usr/lib/openldap
cat >"$WORK/slapd.conf" <<CONF
include $SCHEMA/core.schema
include $SCHEMA/cosine.schema
include $SCHEMA/inetorgperson.schema
include $SCHEMA/nis.schema
pidfile $WORK/slapd.pid
argsfile $WORK/slapd.args
modulepath $MODULES
moduleload back_mdb
# Loaded only for the schema (pwdAccountLockedTime): the overlay is not enabled, so
# a locked account can still bind, as on a directory that leaves that to the client.
moduleload ppolicy
TLSCACertificateFile $WORK/ca.pem
TLSCertificateFile $WORK/server.pem
TLSCertificateKeyFile $WORK/server.key
database mdb
maxsize 10485760
suffix "dc=example,dc=org"
rootdn "cn=admin,dc=example,dc=org"
rootpw admin-password
directory $WORK/data
index objectClass eq
CONF

# Load the data offline: slapadd keeps operational attributes such as the
# ppolicy lock, which cannot be set through the protocol.
slapadd -f "$WORK/slapd.conf" -l "$HERE/ldap/data.ldif" >/dev/null

# A copy of the binary outside the distribution's AppArmor profile, which only
# lets slapd write under /var/lib/ldap.
cp "$(command -v slapd)" "$WORK/slapd-test"
"$WORK/slapd-test" -f "$WORK/slapd.conf" -h "ldaps://127.0.0.1:$LDAPS_PORT ldap://127.0.0.1:$LDAP_PORT" -u "$(id -un)" >"$WORK/slapd.log" 2>&1 || true

for _ in $(seq 1 30); do
  if ldapsearch -x -H "ldap://127.0.0.1:$LDAP_PORT" -b "dc=example,dc=org" -s base >/dev/null 2>&1; then
    cat <<ENV
export HFPROXY_TEST_LDAPS_URL=ldaps://localhost:$LDAPS_PORT
export HFPROXY_TEST_LDAP_URL=ldap://localhost:$LDAP_PORT
export HFPROXY_TEST_LDAP_CA=$WORK/ca.pem
export SSL_CERT_FILE=$WORK/trust.pem
ENV
    exit 0
  fi
  sleep 1
done
echo "slapd did not start:" >&2
cat "$WORK/slapd.log" >&2
exit 1
