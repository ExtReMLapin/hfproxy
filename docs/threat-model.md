# Threat model

hfproxy sits between internal clients and Hugging Face. It authenticates clients, applies per-repository access policies, and caches files in object storage. This page states what it is designed to withstand and what the deployment must provide.

## Assets

- **Upstream token** (`HFPROXY_HF_TOKEN`): one shared credential that can read every private or gated repository its owner can. Clients never see it.
- **Cached files**: models and datasets, possibly licensed or private.
- **Administrator access**: can change policies, tokens, storage and upstream settings.
- **`SESSION_SECRET`**: derives token hashes and the key that encrypts saved settings.

## Trust boundaries

| Source | Trust |
| --- | --- |
| Hugging Face and its CDN | Authenticated by TLS and a redirect host allowlist, but the **content is untrusted**. A repository owner controls file names, types and bytes. |
| Clients | Untrusted until authenticated; then limited by role, token scope and repository policy. |
| Administrators | Trusted, but their sessions are a high-value target. |
| PostgreSQL, object storage, reverse proxy | Trusted infrastructure; the deployment must secure them. |

## Threats and mitigations

| Threat | Mitigation | Residual risk |
| --- | --- | --- |
| A repository file runs script in the console's origin | Files are sent with `Content-Security-Policy: sandbox`, `nosniff`, and HTML/SVG/XML/script files are forced to download | Serve files from a separate hostname for defense in depth |
| Client reads a repository it should not | Policy checked before any cache lookup or upstream call; unregistered repositories are refused | The upstream token's reach is bounded only by local policies |
| Server-side request forgery through redirects or settings | Redirects only to allowlisted HTTPS hosts and ports; the dialer refuses non-public addresses | An administrator can point the Hub URL elsewhere, so treat that role as privileged |
| Tampered or corrupted download | LFS files are verified against the Hub's SHA-256 before they are published | Small non-LFS files carry no digest to verify |
| Stolen read token | Tokens are stored as keyed hashes, expire, can be revoked; a `read` token cannot use the administration API | Anyone holding it can read what its owner can |
| Credential guessing | Argon2id, constant-time comparisons, login limits shared by all instances through PostgreSQL: per address, per address and account, and per account on failures only | Someone with many addresses can still make 50 failed guesses a minute against one account, which also briefly blocks that account |
| Disk or storage exhaustion | Per-file size cap, concurrent fill limit, free-space check, optional total quota with LRU eviction | Size the staging volume for the configured concurrency |
| Upstream outage or rate limiting | Retries with backoff, and cached files are served when a known ref cannot be confirmed (`X-Cache: STALE`) | Uncached files fail until the Hub recovers |
| Compromise or loss of the session secret | Separate derived keys for tokens, sessions and settings; rotation with `HFPROXY_SESSION_SECRET_PREVIOUS` and `hfproxy admin rotate-secret` | Rotate promptly after a leak and revoke old tokens; losing every copy of the secret makes saved settings unreadable |

## Out of scope

hfproxy does not terminate TLS, does not scan model files for malicious content (for example unsafe pickle files), and does not isolate tenants from each other beyond repository policies. Treat downloaded models as untrusted code, exactly as you would when downloading them directly.
