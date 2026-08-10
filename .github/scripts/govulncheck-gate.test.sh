#!/usr/bin/env bash
# Test suite for govulncheck-gate.sh. Fixtures are hand-written slices of
# govulncheck's newline-delimited JSON, carrying only the fields the gate reads.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/govulncheck-gate.sh"
PASS=0
FAIL=0

ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
no() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n     %s\n' "$1" "$2"; }

# Emits one finding. A null function means "imported but not called".
finding() {
  local osv="$1" module="$2" func="$3"
  if [ "$func" = "null" ]; then
    printf '{"finding":{"osv":"%s","trace":[{"module":"%s"}]}}\n' "$osv" "$module"
  else
    printf '{"finding":{"osv":"%s","trace":[{"module":"%s","function":"%s"}]}}\n' \
      "$osv" "$module" "$func"
  fi
}

run_gate() {
  local file="$1"
  OUTPUT="$(bash "$SCRIPT" "$file" 2>&1)"
  STATUS=$?
}

expect_status() {
  local name="$1" want="$2"
  if [ "$STATUS" -eq "$want" ]; then ok "$name"; else
    no "$name" "exit $STATUS, want $want. output: $OUTPUT"
  fi
}

expect_contains() {
  local name="$1" want="$2"
  case "$OUTPUT" in
    *"$want"*) ok "$name" ;;
    *) no "$name" "output does not contain [$want]: $OUTPUT" ;;
  esac
}

echo "govulncheck-gate.sh"
TMP="$(mktemp -d)"

# Only allowlisted docker findings: accepted.
finding GO-2023-1699 github.com/docker/docker "docker.Foo" > "$TMP/docker.json"
finding GO-2022-0390 github.com/docker/docker "docker.Bar" >> "$TMP/docker.json"
run_gate "$TMP/docker.json"
expect_status "allowlisted module alone passes" 0
expect_contains "allowlisted findings are reported" "GO-2023-1699"

# A stdlib finding is exactly what this gate exists to catch.
cp "$TMP/docker.json" "$TMP/stdlib.json"
finding GO-2026-5856 stdlib "tls.Handshake" >> "$TMP/stdlib.json"
run_gate "$TMP/stdlib.json"
expect_status "stdlib finding fails the gate" 1
expect_contains "stdlib finding is named" "GO-2026-5856"

# A finding in any other dependency also fails.
finding GO-2025-9999 golang.org/x/crypto "ssh.Dial" > "$TMP/other.json"
run_gate "$TMP/other.json"
expect_status "non-allowlisted module fails" 1
expect_contains "non-allowlisted module is named" "golang.org/x/crypto"

# Imported-but-not-called findings are informational; govulncheck reports them
# with no function in the trace and the gate must ignore them.
finding GO-2025-8888 golang.org/x/net null > "$TMP/uncalled.json"
run_gate "$TMP/uncalled.json"
expect_status "uncalled finding does not fail the gate" 0

# A clean scan still emits progress/config messages, so the file is non-empty
# but contains no findings.
printf '{"config":{"scanner_name":"govulncheck"}}\n' > "$TMP/clean.json"
run_gate "$TMP/clean.json"
expect_status "no findings passes" 0
expect_contains "clean scan says so" "No reachable vulnerabilities"

# An empty file means govulncheck itself failed; that must not read as a pass.
: > "$TMP/empty.json"
run_gate "$TMP/empty.json"
expect_status "empty input fails loudly" 1

run_gate "$TMP/does-not-exist.json"
expect_status "missing input fails loudly" 1

rm -rf "$TMP"
printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
