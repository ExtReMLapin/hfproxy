# Rotating secrets and certificates

For the session secret, see [Rotating the session secret](operations.md#rotating-the-session-secret): it has its own procedure because it needs two values at once. This page covers the other credentials. Rotate on a schedule you choose, and at once after anyone who knew a secret leaves or a leak is suspected.

## Read this first: where the value comes from

Several credentials can be set in two places: the environment (or the file a `*_FILE` variable points to, which is what the Helm chart's secret does) and the console under **Settings**. **A value saved from the console wins over the environment**, unless the setting is listed in `HFPROXY_LOCKED_SETTINGS`. Two consequences:

- If an old value was ever saved in the console, changing the environment secret changes nothing. Run `hfproxy config show --source`: it says, for each setting, whether it comes from the environment or the console.
- A value saved from the console is applied at once on the instance that received it, but other replicas read it again only when they restart. For a fleet, prefer the environment (or a mounted secret) plus a rolling restart, and lock the setting with `HFPROXY_LOCKED_SETTINGS` so the console cannot override it.

Changing an environment variable or a mounted secret needs a restart: hfproxy reads them at start. With the Helm chart, a changed value in `secrets.files` or `config` rolls the pods; a Secret you edit in place does not, so run `kubectl rollout restart deploy/hfproxy`.

## Hugging Face token (`HFPROXY_HF_TOKEN`)

1. Create a new read-only token on the Hub (the old one keeps working).
2. Store it in your secret and roll out (or save it in **Settings** for a single instance).
3. Check that private or gated repositories still download (`hfproxy_upstream_errors_total{reason="http_4xx"}` must not rise).
4. Revoke the old token on the Hub.

## Object-storage credentials

Use a second access key if your store allows two per identity (AWS IAM, Garage, MinIO do):

1. Create the new key with the same permissions (read, write and delete on the bucket).
2. Update `HFPROXY_S3_ACCESS_KEY` and `HFPROXY_S3_SECRET_KEY` (or the `_FILE` variants) and roll out.
3. When every pod runs with the new key and `hfproxy_storage_errors_total` is flat, delete the old key.

If the store accepts only one key, expect storage errors between the change on the store and the end of the rollout: do it at a quiet time, or scale to one replica first. With an IAM role or another identity the SDK discovers, there is nothing to rotate in hfproxy.

## PostgreSQL password (`HFPROXY_DATABASE_URL`)

Existing connections stay valid when a role's password changes; new ones need the new password. So changing the password on the server first and then restarting is the simplest order, and costs a few seconds of failed requests:

1. `ALTER ROLE hfproxy PASSWORD '…';`
2. Update the secret and roll out immediately.

For no interruption, alternate between two roles: create `hfproxy_b` as a member of the owner role (`CREATE ROLE hfproxy_b LOGIN PASSWORD '…' IN ROLE hfproxy;`), roll the pods onto it, and only then change the first role's password. Run `hfproxy migrate up` (the chart's migration job) as the **owner** role, since schema changes need ownership.

## OIDC client secret (`HFPROXY_OIDC_CLIENT_SECRET`)

An identity provider normally accepts one secret per client, so there is a short window between the change at the provider and the end of the rollout when sign-in through OIDC fails. Existing sessions are not affected. Do it at a quiet time:

1. Generate the new secret at the provider.
2. Update the secret and roll out (or save it in **Settings**, which applies at once on that instance).
3. Check a sign-in.

Some providers can keep two secrets for a while (Keycloak does through a client policy with the secret-rotation executor): if yours does, set the new one first, roll out, then retire the old.

## LDAP bind password (`HFPROXY_LDAP_BIND_PASSWORD`)

Create a second service account with the same read access, switch `HFPROXY_LDAP_BIND_DN` and the password together and roll out, then disable the old account. Changing the password of the same account leaves a window where sign-ins fail with "LDAP service bind failed" in the logs. Directory sign-ins use the user's own bind, which is unaffected.

## Certificates and private certificate authorities

hfproxy checks the certificates of the S3 endpoint, the OIDC issuer, LDAPS and a Hub mirror against the system roots plus the PEM files in `SSL_CERT_DIR` (the chart's `trustedCA`). **The roots are read once, at start**: after changing a CA, restart.

- **A server certificate renewed under the same CA** needs nothing on the hfproxy side.
- **A new CA**: put the new CA certificate in the same ConfigMap or Secret **next to the old one**, roll out, switch the servers to certificates signed by the new CA, check, then remove the old CA and roll out again. Removing the old CA first breaks every connection that still uses it.
- **Expiry.** Alert on the expiry of the certificates of the dependencies you control; hfproxy does not export their expiry date.
- The certificate of the public URL belongs to your reverse proxy or ingress, not to hfproxy.

## API tokens and sessions

- A user's token: **Tokens** in the console, or `hfproxy admin token revoke`. Revoking reaches every replica at once (they are told through PostgreSQL), with `HFPROXY_AUTH_CACHE_TTL` (5 seconds by default) as the upper bound if a notification is lost.
- Ending every session after an incident: rotate the session secret as described in [operations.md](operations.md#rotating-the-session-secret), but without keeping the old one in `HFPROXY_SESSION_SECRET_PREVIOUS`; every token and session then stops working at once.
- The first-start setup code (`HFPROXY_SETUP_TOKEN`) is only used until the first administrator exists. Remove it afterwards.
