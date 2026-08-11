# Development

Building, testing and releasing the driver. Nothing here is needed to *use* it —
see [installation.md](installation.md) for that.

## Repository layout

```
cmd/docker-machine-driver-pve/   Plugin entrypoint (plugin.RegisterDriver)
pkg/driver/                      libmachine Driver implementation
  driver.go                        flags, lifecycle, Create/Remove/state
  diskspec.go                      --pve-data-disk parsing and validation
  guestdisk.go                     guest-side format/mount over SSH
pkg/proxmox/                     go-proxmox wrapper used by the driver
  client.go                        clone, configure, disks, guest agent
  diskspec.go                      DiskSpec/AttachedDisk types
deploy/nodedriver.yaml           Raw NodeDriver manifest
deploy/chart/                    Helm chart Rancher installs the driver from
docs/                            User-facing guides
Makefile                         build / cross-compile / checksums
```

The UI extension lives in a [separate
repository](https://github.com/Lore09/pve-rancher-ui-extension).

## Build and test

```bash
make build                       # build ./docker-machine-driver-pve
make dist                        # cross-compile darwin/linux amd64+arm64 + checksums.txt
make vet test
```

CI additionally runs `go test -race -count=1 ./...` and golangci-lint against
`.golangci.yml`. Run both locally before opening a PR:

```bash
go test -race -count=1 ./...
golangci-lint run
make test-workflows              # unit tests for the release-decision scripts
```

The toolchain floor is **Go 1.25** (`golang.org/x/crypto` requires it) and
golangci-lint **v2** — v1 cannot parse a `go 1.25.0` directive.

### The golden script test

`pkg/driver/testdata/disk-setup.golden.sh` is the exact script the driver runs
as root inside every provisioned node. `TestRenderDiskSetupScriptGolden`
compares the rendered output against it, so any change to that script shows up
as a reviewable diff rather than hiding inside a string builder. If you change
the renderer, update the golden file deliberately — do not regenerate it
without reading the diff.

## Standalone testing with docker-machine

The fastest way to exercise a change without a Rancher server. Install the
plugin on your `PATH` and drive it directly:

```bash
make build && install -m 0755 docker-machine-driver-pve /usr/local/bin/
docker-machine create --driver pve \
  --pve-api-url https://pve.example.com:8006/api2/json \
  --pve-api-token-id rancher@pve!machine \
  --pve-api-token-secret "$(cat token.secret)" \
  --pve-api-insecure \
  --pve-template-vmid 9000 \
  --pve-cores 2 --pve-memory 4096 \
  --pve-cloudinit \
  --pve-data-disk size=10,storage=local-lvm,fs=ext4,mount=/data \
  --pve-ssh-user debian \
  pve-test-node
```

`--pve-keep-on-failure` leaves the half-built VM in place so you can inspect it
instead of watching the rollback destroy the evidence.

## Branches

Two long-lived branches:

- **`dev`** — integration. Open feature pull requests against this branch.
- **`master`** — stable, and the branch Rancher serves the Helm chart from. It
  receives pull requests from `dev` only. It stays the repository default.

### CI tiers

Checks run on pull requests only — never on merge. Branch protection requires a
green PR, so re-running the same commits on merge proves nothing, and
`release.yml` still runs the full suite as its gate.

| Check | PR → `dev` | PR → `master` | Release gate |
|---|---|---|---|
| `Lint (golangci-lint)` | yes | yes | yes |
| `go vet + tests` | yes | yes | yes |
| `govulncheck` | no | yes | yes |
| `Cross-compile` | no | yes | goreleaser does it |

`Chart lint` runs separately on pull requests touching `deploy/chart/**`.

`go test` stays in the `dev` tier deliberately: `dev` is where all feature work
lands, so lint-only there would let a broken test merge cleanly and surface at
release time, when the fix has to travel through another pull request.

Nothing is ever published without the full suite passing, whatever the tier.

## Releasing

**Bump `version` and `appVersion` in
[`deploy/chart/Chart.yaml`](../deploy/chart/Chart.yaml) and merge.** That is the
entire release process. The two fields must match; CI rejects the push if they
drift. The version is always plain `x.y.z` — the `-dev` suffix below belongs to
the tag and is never written into the chart.

The branch decides what kind of release you get:

| Merge into | Chart version | Tag | GitHub release |
|---|---|---|---|
| `dev` | `0.8.0` | `v0.8.0-dev` | prerelease, not marked latest |
| `master` | `0.8.0` | `v0.8.0` | stable, marked latest |

[`chart-release.yml`](../.github/workflows/chart-release.yml) then:

1. Hands the versions to
   [`detect-release.sh`](../.github/scripts/detect-release.sh), which validates
   semver and refuses a decrease or a version whose tag already exists.
2. Creates and pushes the tag.
3. Calls [`release.yml`](../.github/workflows/release.yml), which runs CI as a
   gate, builds the binaries with goreleaser, renders
   `nodedriver-<version>.yaml`, and publishes the GitHub release.
4. Commits the new binary's SHA-256 into `deploy/chart/values.yaml` — onto
   `dev` for a prerelease, onto `master` for a stable release, which then also
   merges `master` back into `dev`.

### Cutting a dev build

Each dev build needs its own bump — `0.8.0`, then `0.8.1`, and so on. Reusing a
version fails the release with "tag already exists".

Install a dev build with the attached manifest, not with Helm:

```bash
kubectl apply -f nodedriver-v0.8.0-dev.yaml
```

Or install the chart from `dev`, naming the dev version explicitly:

```bash
helm install pve-rancher-driver ./deploy/chart --set nodeDriver.version=v0.8.0-dev
```

The prerelease records its digest into `values.yaml` on `dev`, so the checksum
is already correct. Without that `--set` the chart refuses to render, because
its `appVersion` claims `0.8.0` and no such release exists yet.

### Promoting to stable

Open a pull request from `dev` to `master`. Whatever version `Chart.yaml` carries
at merge is the one released — if `dev` iterated through `0.8.0`, `0.8.1` and
`0.8.2`, promoting tags `v0.8.2`, and the intermediate numbers never get stable
tags. That is intended: the tag always reflects the file.

To re-release an existing tag, run **Actions → Release → Run workflow** and
give it the tag. That is the only other way in: `release.yml` has no tag-push
trigger, because a tag pushed with the default `GITHUB_TOKEN` does not start
workflow runs, so that path could never fire on the normal route anyway.

Four details that are load-bearing, in case you edit these workflows:

- `chart-release.yml` watches **`Chart.yaml` only**, and the checksum commit
  writes **`values.yaml` only**. That asymmetry is what stops the release from
  re-triggering itself. Widening the path filter to `deploy/chart/**` creates an
  infinite release loop.
- **`ci.yml`'s concurrency group includes `github.workflow`.** For a reusable
  workflow `github.workflow` is the *caller's* name, which keeps a release's
  gate run (`ci-Chart Release-...`) out of the same bucket as a pull request run
  (`ci-CI-...`). Keying the group on `github.ref` alone put them together and
  `cancel-in-progress` killed one: jobs that start and immediately go grey, and
  occasionally a release whose own gate was cancelled.
- **`ci.yml` has no `paths-ignore`, and `Chart lint` is not a required check.**
  GitHub reports a skipped workflow's checks as *missing* rather than passing.
  A path filter on `ci.yml` would leave chart-only pull requests permanently
  unmergeable against master's required checks, and requiring the path-filtered
  `Chart lint` would block every pull request that does not touch the chart.
- **The tier conditions are written as `github.base_ref == 'master' ||
  inputs.full`.** Not `github.event_name == 'workflow_call'`: on the
  reusable-workflow path `event_name` reports the *caller's* event (`push`).
- **The release gate skips the cross-compile job** (`skip-cross-compile: true`).
  goreleaser rebuilds every target moments later, so running `make dist` first
  only lengthened the release. The `if:` on that job is written as
  `${{ !inputs.skip-cross-compile }}` rather than `== false` on purpose:
  GitHub coerces both `null` and `false` to `0`, so the equality form would
  skip the job on every ordinary push.
- **`release.yml` serialises on one concurrency group and never cancels.** It
  ends by pushing the checksum commit to the default branch; two releases at
  once would race there, and the loser would fail the push having already
  published its binaries.

Between the version bump and the checksum commit there is a short window where
the chart on `master` claims a new version but still carries the previous
version's digest. The chart detects this (`checksumFor` vs the deployed version)
and refuses to render, so the failure is an explanatory error rather than a
driver stuck in `Downloading`.

## Vulnerability scanning

`govulncheck` runs on `master` pull requests and on every release. Its exit code
is not the gate — [`govulncheck-gate.sh`](../.github/scripts/govulncheck-gate.sh)
is, because this module carries findings that cannot be fixed.

`docker/machine v0.16.2` imports `github.com/docker/docker/pkg/term`, which
modern docker releases deleted, so `go.mod` pins `docker/docker` to
`v1.13.1+incompatible`. That pin carries 11 reachable vulnerabilities, three of
which have no fixed version at all. Escaping them means replacing
`docker/machine` — the driver's foundation — not bumping a dependency.

So the gate allowlists **that one module** and fails on everything else. A new
finding in the standard library or any other dependency still breaks the build,
which is the part worth gating on. A stdlib finding is normally fixed by the
`go-version: "1.25"` pin picking up a newer patch release on its own.

Keep the allowlist as short as it can be, and record why each entry is there.

## Adding a flag

1. Declare it in `GetCreateFlags()` and read it in `SetConfigFromFlags`.
2. Validate it in `PreCreateCheck` — failing there is free, failing later means
   a half-built VM to roll back.
3. Add it to the [flag reference](flags.md) and the machine-pool field table in
   [rancher-setup.md](rancher-setup.md).
4. If it should appear in the polished form, add it to the UI extension too —
   the custom Vue form renders only the fields it knows about, so a new flag is
   invisible there until you do. **A flag that is declared but not rendered
   silently takes its default**, which is how `pve-ssh-user` once defaulted to
   `root` on every pool.

Rancher caches the machine-config schema per driver version, so a new flag does
not show up until a chart release carrying the new binary is installed.
