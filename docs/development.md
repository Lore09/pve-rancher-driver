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
```

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

## Releasing

**Bump `version` and `appVersion` in
[`deploy/chart/Chart.yaml`](../deploy/chart/Chart.yaml) and merge to `master`.
That is the entire release process.** The two fields must match; CI rejects the
push if they drift.

[`chart-release.yml`](../.github/workflows/chart-release.yml) then:

1. Diffs the chart version against the pre-push commit, validates semver, and
   refuses a decrease or a version whose tag already exists.
2. Creates and pushes the `v<version>` tag.
3. Calls [`release.yml`](../.github/workflows/release.yml), which runs CI as a
   gate, builds the binaries with goreleaser, renders
   `nodedriver-<version>.yaml`, and publishes the GitHub release.
4. Commits the new binary's SHA-256 back into `deploy/chart/values.yaml`, since
   the digest cannot exist until the binary is built.

To re-release an existing tag, run **Actions → Release → Run workflow** and
give it the tag. That is the only other way in: `release.yml` has no tag-push
trigger, because a tag pushed with the default `GITHUB_TOKEN` does not start
workflow runs, so that path could never fire on the normal route anyway.

Four details that are load-bearing, in case you edit these workflows:

- `chart-release.yml` watches **`Chart.yaml` only**, and the checksum commit
  writes **`values.yaml` only**. That asymmetry is what stops the release from
  re-triggering itself. Widening the path filter to `deploy/chart/**` creates an
  infinite release loop.
- **`ci.yml`'s concurrency group includes `github.workflow`.** A release reaches
  CI twice — once from the push to `master`, once as the gate `release.yml`
  calls — and both runs carry the same `github.ref`. Keying the group on the ref
  alone put them in one bucket, so `cancel-in-progress` killed one of them: jobs
  that start and immediately go grey, and occasionally a release whose own gate
  was cancelled. For a reusable workflow `github.workflow` is the *caller's*
  name, which separates the two.
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
