#!/usr/bin/env bash
# Prints the release notes of a version: docs/releases/VERSION.md when it exists
# (a hand-written summary), otherwise the matching section of CHANGELOG.md.
# VERSION may be "Unreleased". Fails when there is nothing to publish, so a tag
# cannot go out without notes.
set -euo pipefail
version="${1:?usage: release-notes.sh VERSION}"
root="$(cd "$(dirname "$0")/.." && pwd)"
if [ -f "$root/docs/releases/$version.md" ]; then
  cat "$root/docs/releases/$version.md"
  exit 0
fi
notes="$(awk -v v="$version" '
  $0 ~ "^## \\[" v "\\]" { found = 1; next }
  found && /^## \[/ { exit }
  found { print }
' "$root/CHANGELOG.md" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}')"
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
  echo "No release notes for $version: add docs/releases/$version.md or a '## [$version]' section to CHANGELOG.md." >&2
  exit 1
fi
printf '%s\n' "$notes"
