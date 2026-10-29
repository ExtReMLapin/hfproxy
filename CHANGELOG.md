# Changelog

All notable changes to hfproxy. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [semantic versioning](https://semver.org/) (it is below 1.0: minor versions may change behaviour, and say so here). The notes of a release are taken from `docs/releases/VERSION.md` when that file exists, otherwise from the section of this file; see [docs/releasing.md](docs/releasing.md).

## [Unreleased]

### Added

- **First administrator from the console.** A new instance offers a setup page; the setup code is printed at start (or fixed with `HFPROXY_SETUP_TOKEN`), and the page closes once an administrator exists.
- **API and download-path logging with a level that can be changed at run time** (`HFPROXY_LOG_LEVEL`, or Settings → Observability). Errors carry their cause, requests are logged, administrative actions are audited in the log, and the download path logs hits, Hub answers and failures.
- **Download limits**, all off by default and editable in the console: simultaneous downloads per user (or per address), speed per transfer, and a daily quota per user, with a per-user override. A refusal is a `429` with `Retry-After` and is counted in `hfproxy_download_rejected_total`.
- **Token revocation reaches every replica at once** through PostgreSQL notifications (`HFPROXY_AUTH_CACHE_TTL` stays the upper bound).
- **`HFPROXY_HF_ALLOWED_PRIVATE_CIDRS`** to reach a Hub mirror on a private network. Only private ranges are accepted; link-local and loopback can never be allowed.
- **`hfproxy_download_ttfb_seconds{cache}`**, the time to first byte of successful downloads, for latency objectives.
- **Operations documentation**: a runbook per alert, service level objectives with burn-rate alert rules, a sizing guide, a rotation guide for every credential and certificate, and a second Grafana dashboard (resilience and limits). Every alert has a `runbook_url`.
- **A test stack** (`docker-compose.test.yml`) with Keycloak, Prometheus and Grafana.
- **Validation protocols and tools**: `scripts/large-file-test.py` (the real binary with a multi-gigabyte file, reporting memory and staging use), `scripts/backup-restore-drill.sh` (a restore rehearsed on a copy, timed) and [docs/validation.md](docs/validation.md) for a real cluster, the real Hub and a large model.
- **Release pipeline**: a release is created from the tag with binaries for linux/amd64 and linux/arm64, a CycloneDX SBOM, the Helm chart, signed checksums and the notes from this file; the chart is also published as an OCI artifact. The image is checked for bit-for-bit reproducibility in CI.

### Changed

- **LDAP accounts are found by their stable identifier, not by the directory's address.** Changing `ldap://` for `ldaps://`, renaming the server or changing its port used to make every user a stranger (duplicates with auto-provisioning, no sign-in without it). The recorded address now follows the configured one.
- **Connections to the OIDC issuer and the LDAP server are filtered**: cloud metadata services, link-local, unspecified and multicast addresses (and loopback outside development) are refused on the address actually connected to. OIDC calls now have a timeout and follow at most three redirects. Private addresses stay allowed.
- **Dashboard statistics are kept for 15 seconds** per viewer, and computed once for simultaneous requests. Counting hits and misses reads the whole downloads table (0.8 s at five million rows) and the console asked at every page.
- A failed Hub request now logs why (without the URL, which can carry a signature).
- The Helm chart's `appVersion` is kept in line with the latest release.

### Fixed

- Documentation said a Hub mirror "works" without mentioning that private addresses were refused; it now describes the option above.

### Tests and CI

- The chart is installed in a real (kind) cluster and exercised: migration job, first start, cached small and large downloads, a range request, a rolling update to two replicas and a replica killed during use.
- A replica killed in the middle of a cache fill: the interrupted client gets an error and not a corrupted file, and the other replica completes the file.
- The LDAP sign-in is tested against a real OpenLDAP server and the OIDC sign-in against a real Keycloak.

## [0.1.0] - 2026-10-29

A production-hardening release: seven languages for the console, resilient Hub access (retries, circuit breaker, stale-if-error), cache size limit with eviction and pins, stable LDAP identities and group synchronisation, a stricter Content-Security-Policy and key derivation, a Helm chart and a distroless image. Behaviour changes to check before upgrading (public `/metrics` now requires an operator, restricted outbound ports, no default secrets in Compose, new Go module path) are listed in [docs/releases/0.1.0.md](docs/releases/0.1.0.md).

## [0.0.4]

Earlier development versions. See the git history.

[Unreleased]: https://github.com/hfproxy/hfproxy/compare/0.1.0...HEAD
[0.1.0]: https://github.com/hfproxy/hfproxy/compare/0.0.4...0.1.0
[0.0.4]: https://github.com/hfproxy/hfproxy/releases/tag/0.0.4
