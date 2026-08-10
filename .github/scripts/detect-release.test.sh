#!/usr/bin/env bash
# Test suite for detect-release.sh. Plain bash on purpose: bats is not installed
# and the script's contract (env in, key=value out, exit code) needs no harness.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/detect-release.sh"
PASS=0
FAIL=0

# Runs the script with the given environment and captures its output and status.
# Every variable is set explicitly so a leaked value from a previous case cannot
# make a test pass for the wrong reason.
run_case() {
  local branch="$1" new="$2" app="$3" old="$4" tags="$5"
  OUT_FILE="$(mktemp)"
  # No `set +e`/`set -e` dance here: this suite deliberately runs without
  # errexit (see `set -uo pipefail` above), so a non-zero exit from the script
  # under test is captured rather than fatal. Toggling errexit on would leave it
  # enabled for every later case.
  GITHUB_OUTPUT="$OUT_FILE" \
  BRANCH="$branch" NEW_VERSION="$new" APP_VERSION="$app" \
  OLD_VERSION="$old" EXISTING_TAGS="$tags" \
    bash "$SCRIPT" >/dev/null 2>"$OUT_FILE.err"
  STATUS=$?
  OUTPUT="$(cat "$OUT_FILE")"
  STDERR="$(cat "$OUT_FILE.err")"
  rm -f "$OUT_FILE" "$OUT_FILE.err"
}

ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
no() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n     %s\n' "$1" "$2"; }

# Asserts the run succeeded and produced exactly the expected key=value lines.
expect_out() {
  local name="$1" want="$2"
  if [ "$STATUS" -ne 0 ]; then
    no "$name" "exited $STATUS, stderr: $STDERR"; return
  fi
  if [ "$OUTPUT" = "$want" ]; then ok "$name"; else
    no "$name" "got [$OUTPUT] want [$want]"
  fi
}

# Asserts the run failed and the error message mentions the given substring.
expect_fail() {
  local name="$1" want="$2"
  if [ "$STATUS" -eq 0 ]; then
    no "$name" "expected non-zero exit, got 0"; return
  fi
  case "$STDERR" in
    *"$want"*) ok "$name" ;;
    *) no "$name" "stderr [$STDERR] does not contain [$want]" ;;
  esac
}

echo "detect-release.sh"

run_case master 0.8.0 0.8.0 0.7.0 "v0.7.0"
expect_out "master bump produces a stable tag" \
  "$(printf 'bumped=true\ntag=v0.8.0\nprerelease=false')"

run_case dev 0.8.0 0.8.0 0.7.0 "v0.7.0"
expect_out "dev bump produces a -dev prerelease tag" \
  "$(printf 'bumped=true\ntag=v0.8.0-dev\nprerelease=true')"

run_case dev 0.8.1 0.8.1 0.8.0 "v0.7.0
v0.8.0-dev"
expect_out "dev bump past a previous dev tag" \
  "$(printf 'bumped=true\ntag=v0.8.1-dev\nprerelease=true')"

run_case master 0.8.2 0.8.2 0.7.0 "v0.7.0
v0.8.0-dev
v0.8.1-dev
v0.8.2-dev"
expect_out "master promotion skips dev-consumed versions" \
  "$(printf 'bumped=true\ntag=v0.8.2\nprerelease=false')"

run_case master 0.7.0 0.7.0 0.7.0 "v0.7.0"
expect_out "unchanged version does not release" "bumped=false"

run_case dev 0.7.0 0.7.0 0.7.0 "v0.7.0"
expect_out "unchanged version does not release on dev either" "bumped=false"

run_case master 0.8.0 0.8.0 "" ""
expect_out "no previous version is treated as a bump" \
  "$(printf 'bumped=true\ntag=v0.8.0\nprerelease=false')"

run_case master 0.6.0 0.6.0 0.7.0 "v0.7.0"
expect_fail "version decrease is rejected" "went backwards"

run_case dev 0.6.0 0.6.0 0.7.0 "v0.7.0"
expect_fail "version decrease is rejected on dev" "went backwards"

run_case master 0.8.0 0.7.0 0.7.0 "v0.7.0"
expect_fail "version/appVersion mismatch is rejected" "must match"

run_case master "" "" 0.7.0 "v0.7.0"
expect_fail "missing version is rejected" "version is missing"

run_case master null null 0.7.0 "v0.7.0"
expect_fail "literal null version is rejected" "version is missing"

run_case master 0.8 0.8 0.7.0 "v0.7.0"
expect_fail "non-semver version is rejected" "not plain semver"

# The -dev suffix is applied to the tag by this script. Authoring it in the
# chart would yield v0.8.0-dev-dev, so the chart must carry plain x.y.z only.
run_case dev 0.8.0-dev 0.8.0-dev 0.7.0 "v0.7.0"
expect_fail "prerelease suffix in the chart is rejected" "not plain semver"

run_case master 0.8.0-rc.1 0.8.0-rc.1 0.7.0 "v0.7.0"
expect_fail "rc suffix in the chart is rejected" "not plain semver"

run_case master 0.8.0 0.8.0 0.7.0 "v0.7.0
v0.8.0"
expect_fail "reusing an existing stable tag is rejected" "already exists"

run_case dev 0.8.0 0.8.0 0.7.0 "v0.7.0
v0.8.0-dev"
expect_fail "reusing an existing dev tag is rejected" "already exists"

# A stable tag for this version means it already shipped; re-publishing it as a
# prerelease would move backwards.
run_case dev 0.8.0 0.8.0 0.7.0 "v0.7.0
v0.8.0"
expect_fail "dev prerelease of an already-released version is rejected" "already released"

# v0.8.0-dev must not be mistaken for v0.8.0 by a substring match.
run_case master 0.8.0 0.8.0 0.7.0 "v0.7.0
v0.8.0-dev"
expect_out "a dev tag does not block the stable tag" \
  "$(printf 'bumped=true\ntag=v0.8.0\nprerelease=false')"

# sort -V orders 0.9.0 before 0.10.0 only because both are plain x.y.z here.
run_case master 0.10.0 0.10.0 0.9.0 "v0.9.0"
expect_out "double-digit minor is an increase, not a decrease" \
  "$(printf 'bumped=true\ntag=v0.10.0\nprerelease=false')"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
