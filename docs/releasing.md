# Releasing

A release is a git tag `MAJOR.MINOR.PATCH` (no leading `v`; a suffix such as `-rc.1` makes it a pre-release). Two workflows run on the tag:

| Workflow | Produces |
| --- | --- |
| `publish-image.yml` | The multi-architecture image `ghcr.io/hfproxy/hfproxy:VERSION` (and `latest` for a final version), with a build provenance attestation, a BuildKit SBOM and a keyless cosign signature |
| `release.yml` | The GitHub release: `hfproxy_VERSION_linux_{amd64,arm64}.tar.gz` (the binary exactly as in the image), `hfproxy_VERSION_sbom.cdx.json` (CycloneDX, Go dependencies), the Helm chart `hfproxy-VERSION.tgz` (also pushed to `oci://ghcr.io/hfproxy/charts`), `SHA256SUMS` and its signature `SHA256SUMS.sigstore.json` |

## Cutting a release

1. Be on `main` with CI green (all jobs, including `cluster`, `oidc` and `reproducible-image`).
2. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [VERSION] - YYYY-MM-DD`, add a fresh empty `## [Unreleased]` above it and update the links at the bottom. If the release deserves a written summary (behaviour changes, upgrade notes), put it in `docs/releases/VERSION.md`: it is used instead of the changelog section.
3. Set `appVersion` in `deploy/helm/hfproxy/Chart.yaml` to the new version (the packaged chart gets it from the tag anyway, but a chart installed from the repository follows this file).
4. Commit, then tag and push:
   ```bash
   git tag -a VERSION -m "hfproxy VERSION"
   git push origin VERSION
   ```
5. Wait for both workflows. A tag without release notes fails `release.yml` before anything is published (`scripts/release-notes.sh VERSION` is what it runs).
6. The chart package on GHCR is private by default: make it public in the package settings if the chart should be installable by anyone.

If a release workflow fails after the tag was pushed, fix the cause and re-run the job: the release is completed rather than refused if it already exists, and uploads replace files of the same name. Do not move a tag that was published.

## Trying the pipeline without a tag

Push any branch named `release-dry-run/…` (or use *Run workflow* on `release.yml`). It builds both binaries, the SBOM, the chart and the checksums and checks the release-notes script on the `Unreleased` section, but signs nothing and publishes nothing.

## Verifying a release

Replace `VERSION`. The signature identities below are those of this repository's workflows.

```bash
# The image: signature and provenance
cosign verify ghcr.io/hfproxy/hfproxy:VERSION \
  --certificate-identity-regexp '^https://github.com/hfproxy/hfproxy/\.github/workflows/publish-image\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/hfproxy/hfproxy:VERSION --repo hfproxy/hfproxy

# The files of the release
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/hfproxy/hfproxy/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing SHA256SUMS
```

## Reproducibility

The Go binary is built with `-trimpath` and no timestamps; CI builds it twice and compares the bytes. The `reproducible-image` job builds the whole image twice from scratch, with base images pinned by digest and timestamps set to the commit date, and requires the same image digest both times. Rebuilding a release from its tag with the same Docker/BuildKit version should give the same image; a different BuildKit version may lay out layers differently, which the check does not cover.
