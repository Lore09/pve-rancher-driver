#!/usr/bin/env bash
# Decide whether a chart version bump should cut a release, and what to call it.
#
# This is a pure function of its inputs: no git, no network. The calling
# workflow does the git plumbing (reading versions out of commits, listing
# tags) and passes the results in, which is what makes the decision testable
# without a repository. See .github/scripts/detect-release.test.sh.
#
# Inputs (environment):
#   BRANCH        branch the push landed on, e.g. master or dev
#   NEW_VERSION   .version from Chart.yaml at the pushed commit
#   APP_VERSION   .appVersion from Chart.yaml at the pushed commit
#   OLD_VERSION   .version from Chart.yaml at the previous tip; empty if none
#   EXISTING_TAGS newline-separated tags that already exist
#
# Outputs (appended to $GITHUB_OUTPUT, or stdout when unset):
#   bumped=true|false     whether to cut a release at all
#   tag=v<x.y.z>[-dev]    only when bumped=true
#   prerelease=true|false only when bumped=true
set -euo pipefail

CHART=deploy/chart/Chart.yaml

out() { printf '%s\n' "$1" >> "${GITHUB_OUTPUT:-/dev/stdout}"; }
fail() { printf '::error file=%s::%s\n' "$CHART" "$1" >&2; exit 1; }

: "${BRANCH:?BRANCH is required}"
: "${NEW_VERSION:=}"
: "${APP_VERSION:=}"
: "${OLD_VERSION:=}"
: "${EXISTING_TAGS:=}"

# yq prints the string "null" for a missing key rather than an empty string.
#
# Written as `if` rather than `[ x ] && y=`: under `set -e` a trailing AND-list
# whose test fails returns non-zero, which is a well-known errexit footgun. The
# explicit form has no such ambiguity.
if [ "$NEW_VERSION" = "null" ]; then NEW_VERSION=""; fi
if [ "$APP_VERSION" = "null" ]; then APP_VERSION=""; fi
if [ "$OLD_VERSION" = "null" ]; then OLD_VERSION=""; fi

if [ -z "$NEW_VERSION" ]; then
  fail "version is missing"
fi

# appVersion is what the NodeDriver download URL is built from. If it drifts
# from version, the chart and the binary it points at are no longer the same
# release, which is the whole thing this design makes impossible.
if [ "$NEW_VERSION" != "$APP_VERSION" ]; then
  fail "version ($NEW_VERSION) and appVersion ($APP_VERSION) must match"
fi

# Plain x.y.z only. The -dev suffix is applied to the tag below and must never
# be authored in the chart, or the tag becomes v0.8.0-dev-dev. Keeping both
# sides of the comparison suffix-free also means `sort -V` below is exact
# semver ordering rather than the approximation it is for prerelease strings.
if ! printf '%s' "$NEW_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  fail "version '$NEW_VERSION' is not plain semver (x.y.z)"
fi

case "$BRANCH" in
  dev) TAG="v${NEW_VERSION}-dev"; PRERELEASE=true ;;
  *)   TAG="v${NEW_VERSION}";     PRERELEASE=false ;;
esac

if [ "$OLD_VERSION" = "$NEW_VERSION" ]; then
  echo "Chart.yaml changed but version did not; nothing to release." >&2
  out "bumped=false"
  exit 0
fi

# Reject a decrease, which would point the `latest` release at an older binary.
if [ -n "$OLD_VERSION" ]; then
  LOWEST="$(printf '%s\n%s\n' "$OLD_VERSION" "$NEW_VERSION" | sort -V | head -1)"
  if [ "$LOWEST" != "$OLD_VERSION" ]; then
    fail "version went backwards: $OLD_VERSION -> $NEW_VERSION"
  fi
fi

# -F -x: whole-line literal match, so v0.8.0-dev never satisfies a check for
# v0.8.0.
has_tag() { printf '%s\n' "$EXISTING_TAGS" | grep -Fxq "$1"; }

if has_tag "$TAG"; then
  fail "tag $TAG already exists; bump to a new version instead of reusing one"
fi

if [ "$PRERELEASE" = true ] && has_tag "v${NEW_VERSION}"; then
  fail "v${NEW_VERSION} is already released; bump to a new version for a dev prerelease"
fi

out "bumped=true"
out "tag=$TAG"
out "prerelease=$PRERELEASE"
