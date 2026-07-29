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

Two details that are load-bearing, in case you edit these workflows:

- `release.yml` is invoked with `workflow_call`, **not** by its tag trigger. A
  tag pushed using the default `GITHUB_TOKEN` does not start new workflow runs,
  so the tag-triggered path would silently never fire. It is kept as a manual
  escape hatch for re-releasing an existing tag.
- `chart-release.yml` watches **`Chart.yaml` only**, and the checksum commit
  writes **`values.yaml` only**. That asymmetry is what stops the release from
  re-triggering itself. Widening the path filter to `deploy/chart/**` creates an
  infinite release loop.

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
