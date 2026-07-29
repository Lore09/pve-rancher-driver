# pve-rancher-driver

A [Rancher node driver](https://ranchermanager.docs.rancher.com/pages-for-subheaders/node-drivers-and-node-templates)
for [Proxmox VE](https://www.proxmox.com/en/proxmox-virtual-environment/overview),
driver name **`pve`**. It lets an RKE2 or K3s cluster in Rancher provision,
scale and remove Proxmox VE virtual machines as machine-pool nodes by cloning
an existing VM template, booting it, and waiting for the QEMU guest agent to
report the cloned VM's IP. Authentication to PVE is via an API token (no root
password ever lands in a cloud credential).

## How it works

```
Rancher UI   ──┐
                ├─► docker-machine-driver-pve (this binary)
                │       │
PVE REST API ◄──┘       ├─ Resolve target node (or first online node)
                        ├─ Generate the machine's SSH keypair
                        ├─ Clone template VMID -> new VMID
                        ├─ Apply overrides (cores, sockets, memory, onboot,
                        │   optional cloud-init ipconfig0 / ciuser / sshkeys
                        │   incl. the generated public key)
                        ├─ Attach the declared data disks (serial-tagged)
                        ├─ Start the VM
                        ├─ Capture the NIC MAC, then poll the QEMU guest
                        │   agent for the IPv4 on that exact interface
                        └─ SSH in and format/mount the data disks
                            → returns "created" once the disks are mounted
```

Detailed guides live in [`docs/`](docs/):

- **[docs/template-preparation.md](docs/template-preparation.md)** — build a
  ready-to-clone PVE template from **Debian 13 (trixie)** or
  **openSUSE Leap Micro 6.2** (immutable), plus the guest packages Longhorn
  needs.
- **[docs/networking.md](docs/networking.md)** — a controlled DHCP node network
  on a PVE host whose LAN bridge is served by a virtualized OPNsense, either as
  a host-local bridge or a tagged VLAN.
- **[docs/rancher-setup.md](docs/rancher-setup.md)** — register the driver,
  create the cloud credential, size machine pools, and troubleshoot.

Every flag the driver declares in `GetCreateFlags()` becomes a node-template
field in the Rancher UI automatically (no separate UI bundle is needed in
modern Rancher — the `NodeDriver` resource asks the binary for its flags).

## Project layout

```
cmd/docker-machine-driver-pve/   Plugin entrypoint (plugin.RegisterDriver)
pkg/driver/                      libmachine Driver implementation
pkg/proxmox/                     go-proxmox wrapper used by the driver
deploy/nodedriver.yaml           Rancher NodeDriver CRD to apply
deploy/chart/                    Helm chart Rancher installs the driver from
docs/                            template prep + Rancher integration guides
Makefile                         build / cross-compile / checksums
```

## Prerequisites

### Proxmox VE API token

Create a dedicated user and a token, then grant a least-privilege role **to
both the user and the token** (PVE tokens do not inherit the user's ACLs
unless `--privsep 0` is set). The required privilege set differs between
PVE majors: PVE 9.x replaced the broad `VM.Monitor` with the more narrowly
scoped `VM.GuestAgent.Audit` for reading guest-agent data. The driver
probes the live server version in `PreCreateCheck` and only accepts the
matching set.

**PVE 9.x** (and later):

```bash
pveum role add RancherPVENode -privs "VM.Clone,VM.Allocate,VM.Audit,VM.PowerMgmt,VM.Config.Disk,VM.Config.CPU,VM.Config.Memory,VM.Config.Network,VM.Config.Cloudinit,VM.Config.Options,VM.GuestAgent.Audit,Datastore.AllocateSpace,Datastore.Audit,SDN.Use,Pool.Allocate"
```

**PVE 8.x** (still supported until its EOL on 2026-08-31):

```bash
pveum role add RancherPVENode -privs "VM.Clone,VM.Allocate,VM.Audit,VM.PowerMgmt,VM.Config.Disk,VM.Config.CPU,VM.Config.Memory,VM.Config.Network,VM.Config.Cloudinit,VM.Config.Options,VM.Monitor,Datastore.AllocateSpace,Datastore.Audit,SDN.Use,Pool.Allocate"
```

Then, for either version:

```bash
pveum user add rancher@pve
pveum user token add rancher@pve machine
pveum acl modify / -user rancher@pve -role RancherPVENode
pveum acl modify / -token 'rancher@pve!machine' -role RancherPVENode
```

The driver's `pve-api-token-id` is `rancher@pve!machine`; the
`pve-api-token-secret` is printed once by `pveum user token add` — save it,
it is not shown again.

> If you have a single role already defined and want to cover both PVE 8 and
> 9 clusters, grant the union (both `VM.Monitor` and `VM.GuestAgent.Audit`).
> The extra priv is harmless on the version that does not require it.

### VM template

VMs are produced by cloning a template, so the template must be ready before
you point the driver at it. The short version of the requirements:

1. `qemu-guest-agent` must be **baked into** the image (the driver polls it
   the moment the clone boots; installing it via a first-boot cloud-init
   script is too late).
2. The VM needs a cloud-init drive (`--ide2 <storage>:cloudinit`) so the
   driver can inject network config, the cloud-init user and the machine's
   SSH public key.
3. The cloud-init user (`--ssh-user`) needs passwordless `sudo` plus `curl`
   and `bash` on `PATH` so Rancher's system-agent bootstrap can finish after
   `Create` returns.

Step-by-step recipes, including a smoke test to run before involving Rancher:

- **[Debian 13 (trixie) genericcloud](docs/template-preparation.md#option-a--debian-13-trixie)** — recommended default
- **[openSUSE Leap Micro 6.2 Default-qcow](docs/template-preparation.md#option-b--opensuse-leap-micro-62-immutable)** — immutable/transactional base

### Data disks (Longhorn etc.)

A machine pool can declare any number of data disks. Each one is attached to the
cloned VM and, unless you ask for `fs=none`, formatted and mounted by the driver
before `Create` returns — so a node never joins the cluster with its storage
directory unmounted:

```bash
--pve-data-disk size=100,storage=local-lvm,fs=ext4,mount=/var/lib/longhorn \
--pve-data-disk size=50,storage=ceph-rbd,fs=xfs,mount=/var/lib/rancher
```

Each disk is stamped with a PVE disk `serial` (its `label`, `pvedata<N>` by
default), which is how the driver finds it in the guest — kernel names like
`sdb` and `sdc` are assigned in discovery order and are not stable once a VM has
more than one data disk. Formatting happens over SSH with the keypair
cloud-init injects, so `--pve-cloudinit` is required for any disk that gets a
filesystem; `PreCreateCheck` rejects the combination rather than cloning a VM it
cannot finish.

The operation is idempotent: a disk that already carries a filesystem is never
reformatted, and an fstab entry is never duplicated. Data disks default to
`backup=0`, since a replicated Longhorn volume gains nothing from a host-level
backup; set `backup=1` per disk when PVE should back it up.

Use `fs=none` to attach a raw block device and leave the guest untouched.

Only give data disks to pools that run the storage provisioner (workers), not to
control-plane/etcd pools. The template still has to carry the packages the
storage stack needs — see
[docs/template-preparation.md](docs/template-preparation.md#guest-dependencies-for-longhorn).

## Build

```bash
make build                       # build ./docker-machine-driver-pve
make dist                        # cross-compile darwin/linux amd64+arm64 + checksums.txt
make vet test
```

## Register in Rancher

Apply the bundled manifest against the **Rancher local cluster**:

```bash
kubectl apply -f deploy/nodedriver.yaml
```

First edit `deploy/nodedriver.yaml` and replace `<VERSION>` and `<SHA256>`
with the matching line from `dist/checksums.txt` for
`docker-machine-driver-pve-linux-amd64` (the OS/arch of the host running the
Rancher management plane). `whitelistDomains` already lists all three GitHub
release redirect hosts — don't drop any, or the download silently stalls.

Also **add your PVE hostname** to `whitelistDomains` (hostname only, no scheme
and no `:8006`). The same list gates Rancher's `/meta/proxy`, which the UI
extension uses for **Test Connection** and the template/storage/bridge
dropdowns; without it Test Connection reports that Rancher could not reach the
Proxmox VE server. Provisioning itself does not use that proxy.

That proxy also verifies PVE's TLS certificate against the Rancher server's
trust store, which a stock PVE certificate fails — the credential's `Insecure
TLS` / `CA Cert` apply to the driver only. Either
[add the PVE CA to Rancher](docs/rancher-setup.md#make-rancher-trust-the-proxmox-ve-certificate)
or accept the warning and type the machine-pool fields by hand.

Alternatively, paste the URL and checksum directly through the UI at
**Cluster Management → Drivers → Node Drivers → Add Node Driver**.

The full walkthrough — cloud credential creation, machine-pool field guide,
verification commands and a troubleshooting matrix — is in
**[docs/rancher-setup.md](docs/rancher-setup.md)**.

## Standalone testing with docker-machine

For local debugging without Rancher, install the plugin on your `PATH` and
invoke it through docker-machine:

```bash
make build && install -m 0755 docker-machine-driver-pve /usr/local/bin/
docker-machine create --driver pve \
  --pve-api-url https://pve.example.com:8006/api2/json \
  --pve-api-token-id rancher@pve!machine \
  --pve-api-token-secret "$(cat token.secret)" \
  --pve-api-insecure \
  --pve-template-vmid 9000 \
  --pve-cores 2 --pve-memory 4096 \
  --pve-cloudinit --pve-ipconfig ip=dhcp \
  --pve-ciuser debian \
  --ssh-user debian \
  pve-test-node
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `pve-api-url` | *(required)* | PVE REST API base URL, e.g. `https://host:8006/api2/json` |
| `pve-api-token-id` | *(required)* | PVE API token id (`USER@REALM!TOKENID`) |
| `pve-api-token-secret` | *(required)* | PVE API token secret |
| `pve-api-insecure` | `false` | Skip TLS certificate verification |
| `pve-ca-cert` | *(empty)* | PEM CA certificate content to trust for the PVE API |
| `pve-node` | *(first online)* | Target PVE node name |
| `pve-vmid` | `0` | Explicit VMID for the created VM, `0` = auto-assigned |
| `pve-template-vmid` | *(required)* | Template VMID to clone from |
| `pve-vmname-prefix` | *(empty)* | Prefix for the PVE VM name, rendered as `<prefix>-<machine name>`. Empty uses the machine name unchanged. Letters, digits and inner hyphens only — PVE validates the result as a DNS name |
| `pve-cores` | `2` | CPU cores per socket |
| `pve-sockets` | `1` | CPU sockets |
| `pve-memory` | `2048` | RAM in MB |
| `pve-boot-disk-size` | `0` | Grow the cloned boot disk to this size in GB (`0` = keep the template's size). PVE can only grow a disk, never shrink it |
| `pve-boot-disk-device` | `scsi0` | PVE config key of the boot disk grown by `pve-boot-disk-size` (`scsi0`, `virtio0`, `sata0`, ...) |
| `pve-data-disk` | *(empty)* | Data disk to attach; **repeatable**. `size=<GB>,storage=<pve-storage>[,fs=ext4\|xfs\|none][,mount=<abs path>][,label=<name>][,device=scsi1..scsi30][,discard=on\|off][,iothread=0\|1][,backup=0\|1]`. Unless `fs=none`, the driver formats the disk and mounts it at `mount=` |
| `pve-disk-setup-timeout` | `300` | Seconds to wait for SSH plus formatting and mounting of the data disks |
| `pve-net-iface` | *(empty)* | Restrict IP discovery to this guest interface name |
| `pve-net-device` | `net0` | PVE config device (`net0`..`net31`) whose MAC pins down IP discovery, and which the `pve-net-*` settings below are written to |
| `pve-net-bridge` | *(empty)* | PVE bridge to attach the NIC to, e.g. `vmbr1`. **Empty leaves the template's network untouched**; setting it rewrites `pve-net-device` |
| `pve-net-model` | `virtio` | Emulated NIC model, applied only when `pve-net-bridge` is set |
| `pve-net-vlan-tag` | `0` | 802.1Q VLAN tag (`0` = untagged). Requires `pve-net-bridge` |
| `pve-net-mtu` | `0` | NIC MTU (`0` = PVE default). Requires `pve-net-bridge` |
| `pve-net-firewall` | *(empty)* | `true`/`false` to toggle the PVE firewall on the NIC; empty keeps the PVE default. Requires `pve-net-bridge` |
| `pve-agent-timeout` | `300` | Seconds to wait for the QEMU guest agent to report an IP |
| `pve-skip-permission-check` | `false` | Skip the token-permission probe in `PreCreateCheck` |
| `pve-keep-on-failure` | `false` | Leave the cloned VM in place when Create fails (debugging only) |
| `pve-cloudinit` | `false` | Push `ipconfig0` / `ciuser` / `sshkeys` to the cloned VM |
| `pve-ipconfig` | `ip=dhcp` | Cloud-init `ipconfig0` value |
| `pve-ciuser` | *(empty)* | Cloud-init user to create/configure with the SSH keys (use `rancher` for Leap Micro; empty = image default user, e.g. `debian`) |
| `pve-sshkeys` | *(empty)* | Extra OpenSSH public keys, one per line (the machine's own generated key is always injected) |
| `pve-onboot` | `false` | Start the VM automatically on PVE boot |
| `ssh-user` | `root` | SSH user used to log into the VM |
| `ssh-port` | `22` | SSH port used to log into the VM |

## Releasing

**Bump `version` and `appVersion` in
[`deploy/chart/Chart.yaml`](deploy/chart/Chart.yaml) and merge to `master`.
That is the entire release process.** The two fields must match; CI rejects the
push if they drift.

[`chart-release.yml`](.github/workflows/chart-release.yml) then:

1. Diffs the chart version against the pre-push commit, validates semver, and
   refuses a decrease or a version whose tag already exists.
2. Creates and pushes the `v<version>` tag.
3. Calls [`release.yml`](.github/workflows/release.yml), which runs CI as a
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

## Compatibility

- **Rancher**: v2.x with `management.cattle.io/v3` `NodeDriver` support.
- **Proxmox VE**: 8.x and 9.x, when the API token above is granted the role.

## License

MIT — see `LICENSE`.