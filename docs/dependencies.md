# Dependencies

Dependabot proposes updates weekly for Go modules, npm packages (the console), GitHub Actions, the Dockerfile's base images and the Compose files' images ([`.github/dependabot.yml`](../.github/dependabot.yml)). CI (build, tests, `staticcheck`, `gosec`, `govulncheck`, the image scan, the cluster and identity-provider tests, the reproducible-image check) must pass before anything is merged.

## Rules

- **Read before merging.** Dependabot opens one pull request per group (Go, Angular, the other npm packages). Check the changelog of the packages with a major version change.
- **Delay.** A brand new release is proposed only after 5 days (14 for a major version), because a compromised release is usually withdrawn within days. Security updates are never delayed.
- **Base images are pinned by digest** in the `Dockerfile` (and actions by commit SHA in the workflows). A tag alone can be moved; a digest cannot. Dependabot proposes the new digest together with the tag.
- **Vulnerabilities** are found by `govulncheck` (Go), `npm audit` (production dependencies of the console) and Trivy (the image). A finding blocks CI; fix it by updating, not by ignoring, unless the vulnerable code is not reachable (then record why in [security-scanning.md](security-scanning.md)).
- **No dependency is added casually.** Prefer the standard library; a new module needs a reason in its pull request.

## TypeScript is held back

The Angular compiler declares the TypeScript versions it accepts (`@angular/compiler-cli` currently requires `>=6.0 <6.1`). A newer TypeScript, such as the 7.x pull requests Dependabot opened, fails the build, so those pull requests are closed and `dependabot.yml` ignores TypeScript major and minor updates. When Angular's range includes a newer TypeScript, update Angular first, then remove the `ignore` entry and bump TypeScript. `typescript-eslint` follows TypeScript: update it only to a version that supports the one in use.

## Where the dependencies are listed

- The Go modules and versions of a release: its CycloneDX SBOM (`hfproxy_VERSION_sbom.cdx.json`), and the SBOM attached to the image.
- The console's npm packages are compiled into static files; `npm audit --omit=dev` in CI covers what ships.
