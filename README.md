# hfproxy

hfproxy is an internal Hugging Face Hub proxy written in Go. It applies local access policies before every request, fetches files from the Hub, deduplicates them by SHA-256 in a private S3 or MinIO bucket, and stores the catalog in PostgreSQL.

The implementation covers the main read and operations paths. Features outside the current MVP are listed below.

## Run locally

The provided Compose setup is intended for development on a trusted machine. It uses demo passwords, HTTP on the internal Docker network, and allows anonymous reads only for repositories marked public.

    cp .env.example .env
    docker compose up --build -d

Create the first administrator. Put the password in a protected file or send it on stdin; it is never passed as a process argument.

    mkdir -p .secrets
    umask 077
    # Write the password to .secrets/admin-password using your secrets manager.
    docker compose exec -T hfproxy hfproxy admin create-user --username admin --password-file - < .secrets/admin-password

Register a repository and choose its policy before using it:

    docker compose exec hfproxy hfproxy admin repo add --type models --id bert-base-uncased --policy public

Open http://localhost:8080 and sign in. The overview shows the cache, policies, and recent artifacts. For HF clients, create an hfproxy token; its full value is shown only once:

    docker compose exec hfproxy hfproxy admin token create --username admin --name laptop

Then configure a Python client:

    export HF_ENDPOINT=http://localhost:8080
    export HF_TOKEN=hfpx_your_hfproxy_token
    python -c 'from huggingface_hub import hf_hub_download; print(hf_hub_download("bert-base-uncased", "config.json"))'

When `HFPROXY_ANONYMOUS_PUBLIC_READ=true`, clients can read repositories registered with the public policy without a token. The administration endpoints and dashboard still require authentication.

## Commands

    hfproxy serve
    hfproxy migrate up|status
    hfproxy admin create-user --username NAME --password-file PATH
    hfproxy admin token create|list|revoke
    hfproxy admin repo add
    hfproxy admin identity link
    hfproxy cache inspect|purge|verify
    hfproxy config validate
    hfproxy version

`cache purge` is a dry run by default; execution requires `--execute --yes`. `cache verify` reads the objects and checks their SHA-256 hashes. Secrets can be supplied through `HFPROXY_*_FILE`. `config validate` does not reveal configuration values.

## Configuration

Environment variables use the `HFPROXY_` prefix. Common settings for local Compose are defined in `.env.example`.

| Area | Main variables |
| --- | --- |
| HTTP and database | LISTEN_ADDR, PUBLIC_URL, DATABASE_URL, AUTO_MIGRATE, LOG_LEVEL |
| Hugging Face | HF_BASE_URL, HF_TOKEN, HF_ALLOWED_REDIRECT_HOSTS, HF_TIMEOUT |
| S3 | S3_ENDPOINT, S3_REGION, S3_BUCKET, S3_PREFIX, S3_FORCE_PATH_STYLE, S3_ACCESS_KEY, S3_SECRET_KEY |
| Authentication | AUTH_MODE=local or oidc, SESSION_SECRET, ANONYMOUS_PUBLIC_READ |
| OIDC | OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_REDIRECT_URL, OIDC_AUTO_PROVISION |
| LDAP | LDAP_URL, LDAP_STARTTLS, LDAP_BIND_DN, LDAP_BIND_PASSWORD, LDAP_BASE_DN, LDAP_USER_FILTER, LDAP_AUTO_PROVISION |
| Cache limits | CACHE_MAX_FILE_SIZE, CACHE_REF_TTL, CACHE_NEGATIVE_TTL, CACHE_GC_GRACE |

PostgreSQL is required. The S3 bucket must exist, remain private, and allow the service identity to read, write, and delete objects. Standard AWS credentials (IAM role, AWS environment variables, or a mounted profile) work when the S3 access key variables are unset. `S3_FORCE_PATH_STYLE=true` is usually required for MinIO.

The upstream HF token is optional for public repositories and required for private or gated repositories. It is sent only to the configured HF host, never to the client. Redirects are followed over HTTPS to allowed HF hosts. Add the delivery hosts observed in your environment to `HF_ALLOWED_REDIRECT_HOSTS`.

### OIDC and LDAP

OIDC uses Authorization Code, PKCE S256, state, nonce, and ID token validation. By default, an OIDC identity must be linked to a local account by an administrator using the CLI. Optional auto-provisioning creates reader accounts only and requires a verified email address.

    hfproxy admin identity link --provider oidc --username admin --issuer https://idp.example/issuer --subject SUBJECT_FROM_ID_TOKEN

LDAP supports LDAPS or LDAP with StartTLS. The user filter must contain `{username}`; the value is escaped before the search. The proxy searches for a single entry, then binds with the supplied password. It does not retain that password. LDAP auto-provisioning is disabled by default; when enabled, new accounts receive the reader role.

OIDC and LDAP provide sign-in to the same local role model (`admin`, `operator`, `reader`). Groups can be managed locally and granted access through allowlist policies.

## Hub API and compatibility

Supported Hub read routes:

- GET and HEAD `/{models|datasets|spaces}/{repo_id}/resolve/{revision}/{filepath}`
- GET `/api/{models|datasets|spaces}/{repo_id}`
- GET `/api/{type}/{repo_id}/revision/{revision}`
- GET `/api/{type}/{repo_id}/tree/{revision}[/{path}]`
- GET `/api/{type}/{repo_id}/commits/{revision}`
- GET `/api/whoami-v2` and `/api/health`

The proxy supports branch, tag, and SHA revisions; nested paths; HEAD; ETag/304; and simple Range requests from the cache. Mutable references are cached for `CACHE_REF_TTL` (60 seconds by default), while a SHA remains immutable. Multi-part ranges are explicitly rejected. Hub metadata uses an allowlist of routes; there is no generic URL proxy.

The operations API is under `/api/v1` and covers identity and providers, statistics, repositories and grants, cache, users, groups (`GET`/`POST /groups`, `PATCH /groups/{id}`), audit, and public settings. Errors use `application/problem+json`. Administrative operations require the admin role.

A cache hit never bypasses authorization. Anonymous reads apply only to repositories explicitly registered as public. An allowlist policy grants access to explicitly authorized local users and groups; `admin_only` is the default policy.

## Containers and operations

The multi-stage Dockerfile builds a static binary and runs the final image as UID 65532. `docker-compose.yml` is a local development environment with PostgreSQL and MinIO; it binds the proxy port to loopback only. `docker-compose.example.yml` demonstrates secret injection and TLS termination by a reverse proxy.

For production:

1. Set `PUBLIC_URL` and the OIDC callback to HTTPS URLs.
2. Disable `ANONYMOUS_PUBLIC_READ`, protect secrets with a dedicated secrets manager, and keep the bucket private.
3. Apply migrations with `hfproxy migrate up` during deployment; `AUTO_MIGRATE` is disabled by default.
4. Provide enough local temporary disk space for the configured maximum file size: cache misses are staged on disk before being published to S3.
5. Back up PostgreSQL and the bucket together. Restore both to preserve the links between artifacts and blobs.
6. Restrict egress to Hugging Face and validated HF CDN hosts. The upstream token should have only the minimum scopes and gated access required.

Health endpoints: `/healthz` provides liveness without dependency checks; `/readyz` checks PostgreSQL and S3. JSON logs include request ID, route, duration, status, and bytes, without query strings, bearer tokens, or signed URLs.

## Current limitations

- Native Xet/CAS, uploads, repository write operations, inference, and Spaces execution are not supported.
- The reference TTL applies to cache reads; a cache miss may still query the Hub API for metadata and to confirm the SHA before downloading.
- OIDC verifies identities and sessions, but IdP group claims are not automatically mapped to local groups.
- LDAP supports LDAPS/StartTLS and user binds; Active Directory group synchronization is not included.
- The dashboard provides an overview and repository registration. Detailed user, group, cache, and audit management is available through the API rather than dedicated pages.
- Multi-part Range requests, Prometheus metrics, distributed rate limits, periodic orphan-blob collection, and paginated cache purge still need work before large-scale operation.
- Negative-result caching is not implemented yet; `CACHE_NEGATIVE_TTL` and `CACHE_GC_GRACE` are reserved settings and currently have no effect.
- Public policies control local access; they do not replace Hugging Face license terms or gated-access conditions.

Do not claim Xet or full Transformers compatibility without running a client-version matrix against representative repositories.
