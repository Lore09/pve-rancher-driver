#!/usr/bin/env bash
# Turn govulncheck's JSON output into a pass/fail gate.
#
# Plain `govulncheck ./...` cannot be a required check on this repository: it
# reports 11 vulnerabilities in github.com/docker/docker@v1.13.1, and that pin
# is not removable. docker/machine v0.16.2 imports
# github.com/docker/docker/pkg/term, which modern docker releases deleted, so
# the version is held back deliberately — see the comment in go.mod. Three of
# the findings have no fixed version at all. Gating on them would block every
# pull request forever while telling us nothing we can act on.
#
# So: fail on any *called* vulnerability outside the allowlist, and report the
# allowlisted ones as notices. A new finding in the standard library or in any
# other dependency still fails the build, which is the part worth gating on.
#
# "Called" means govulncheck traced an actual call path from this module's code
# into the vulnerable symbol. Findings that only appear in imported packages or
# required modules are informational and are not gated.
#
# Usage: govulncheck-gate.sh <govulncheck-json-file>
set -euo pipefail

JSON="${1:?usage: govulncheck-gate.sh <govulncheck-json-file>}"

# Modules whose findings are known, unfixable, and accepted. Keep this list as
# short as it can possibly be, and record why each entry is here.
#
#   github.com/docker/docker — pinned to v1.13.1+incompatible by
#     docker/machine's use of pkg/term. Removing it means replacing
#     docker/machine, which is the driver's foundation. See go.mod.
ALLOWED_MODULES=(
  "github.com/docker/docker"
)

if [ ! -s "$JSON" ]; then
  echo "::error::$JSON is empty or missing; govulncheck produced no output"
  exit 1
fi

# trace[0] is the vulnerable frame. A non-null function there is what
# distinguishes a reachable call from a mere import.
CALLED="$(jq -rc '
  select(.finding)
  | .finding
  | select(.trace[0].function != null)
  | [.osv, .trace[0].module]
  | @tsv
' "$JSON" | sort -u)"

BLOCKING=""
ALLOWED=""
while IFS=$'\t' read -r OSV MODULE; do
  [ -n "$OSV" ] || continue
  SKIP=false
  for A in "${ALLOWED_MODULES[@]}"; do
    if [ "$MODULE" = "$A" ]; then SKIP=true; break; fi
  done
  if [ "$SKIP" = true ]; then
    ALLOWED="${ALLOWED}${OSV} (${MODULE})"$'\n'
  else
    BLOCKING="${BLOCKING}${OSV} (${MODULE})"$'\n'
  fi
done <<< "$CALLED"

if [ -n "$ALLOWED" ]; then
  echo "Accepted (allowlisted module, see script header):"
  printf '%s' "$ALLOWED" | sed 's/^/  - /'
fi

if [ -n "$BLOCKING" ]; then
  echo "::error::govulncheck found reachable vulnerabilities outside the allowlist:"
  printf '%s' "$BLOCKING" | sed 's/^/  - /'
  echo "Fix them by updating the affected module, or bump the Go toolchain for a stdlib finding."
  exit 1
fi

echo "No reachable vulnerabilities outside the allowlist."
