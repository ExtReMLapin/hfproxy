# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's private vulnerability reporting for this repository (Security tab, "Report a vulnerability"), and include the version, the configuration involved and the steps to reproduce.

You can expect an acknowledgement within a few working days. Fixes are released as a new patch version and credited in the release notes unless you prefer otherwise.

## Supported versions

Only the latest release receives security fixes. hfproxy is pre-1.0: upgrade to the newest patch version rather than expecting backports.

## Hardening and design notes

- [docs/production.md](docs/production.md) lists the settings a shared deployment needs.
- [docs/threat-model.md](docs/threat-model.md) describes what hfproxy defends against and what it leaves to the deployment.
