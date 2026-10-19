#!/usr/bin/env bash
# Prints the version HEAD should be released as, or nothing when HEAD is already released.
# A VERSION bumped by hand (make bump) wins; otherwise the Conventional Commits since the last
# tag decide: a breaking change (type!: or BREAKING CHANGE:) bumps major, feat minor, the rest patch.
set -euo pipefail

semver='^[0-9]+\.[0-9]+\.[0-9]+$'
cur="$(cat VERSION)"
[[ "$cur" =~ $semver ]] || { echo "VERSION '$cur' is not X.Y.Z" >&2; exit 1; }

tag="$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' 2>/dev/null || true)"
if [ -z "$tag" ]; then
  echo "$cur"
  exit 0
fi
[ "$(git rev-parse "$tag^{commit}")" = "$(git rev-parse HEAD)" ] && exit 0

last="${tag#v}"
if [ "$cur" != "$last" ] && [ "$(printf '%s\n%s\n' "$last" "$cur" | sort -V | tail -n1)" = "$cur" ]; then
  echo "$cur"
  exit 0
fi

log="$(git log --format='%s%n%b' "$tag..HEAD")"
IFS=. read -r major minor patch <<<"$last"
if grep -Eq '^[a-z]+(\([^)]*\))?!:|^BREAKING[ -]CHANGE:' <<<"$log"; then
  echo "$((major + 1)).0.0"
elif grep -Eq '^feat(\([^)]*\))?:' <<<"$log"; then
  echo "$major.$((minor + 1)).0"
else
  echo "$major.$minor.$((patch + 1))"
fi
