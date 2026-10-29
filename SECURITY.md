# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's private vulnerability reporting for this repository (Security tab, "Report a vulnerability"), and include the version, the configuration involved (with secrets removed) and the steps to reproduce, and say whether the problem needs an authenticated user, an administrator or a malicious Hub.

You can expect an acknowledgement within a few working days. Fixes are released as a new patch version and credited in the release notes unless you prefer otherwise.

## Supported versions

Only the latest release receives security fixes. hfproxy is pre-1.0: upgrade to the newest patch version rather than expecting backports.

## In scope

The hfproxy service, its Helm chart and container image, and the console. Issues in a dependency go to that project first; tell us as well if hfproxy is exposed in a way its own advisory does not cover. Out of scope: the content of models you download (see the [threat model](docs/threat-model.md)), and a deployment that ignores [docs/production.md](docs/production.md) (public HTTP, shared default credentials, the metrics port open to everyone).

## If you suspect a leak

Rotate the affected credential with [docs/rotation.md](docs/rotation.md), revoke tokens (`hfproxy admin token revoke`) and read the audit log for the period. [docs/runbooks.md](docs/runbooks.md) covers the alerts that may accompany it.

## Hardening and design notes

- [docs/production.md](docs/production.md) lists the settings a shared deployment needs.
- [docs/threat-model.md](docs/threat-model.md) describes what hfproxy defends against and what it leaves to the deployment.
