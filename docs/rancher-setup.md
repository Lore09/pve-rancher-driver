# Integrating the `pve` node driver with Rancher

This walks the whole path from a built driver binary to a working RKE2/K3s
cluster whose nodes are Proxmox VE VMs. It assumes:

- Rancher **v2.x** (management.cattle.io/v3 NodeDriver support) and `kubectl`
  access to the **local** (management) cluster.
- A Proxmox VE cluster reachable from the Rancher pods, with the API token
  from the [README](../README.md#proxmox-ve-api-token) and a template from
  [template-preparation.md](template-preparation.md).
- This repo checked out, or the release artifacts downloaded.

## 1. Build (or fetch) the driver binary

Rancher downloads the driver binary from the URL in the NodeDriver resource,
so it must be hosted somewhere the Rancher pods can reach — typically a
GitHub release of this repo.

```bash
make dist          # produces dist/docker-machine-driver-pve-<os>-<arch> + checksums.txt
cat dist/checksums.txt | grep linux-amd64
```

If you tag a release (`git tag v0.1.0 && git push origin v0.1.0`), the
[release workflow](../.github/workflows/release.yml) does this for you and
attaches a **pre-rendered `nodedriver-<version>.yaml`** to the GitHub release
— if you use that file you can skip step 2 entirely.

## 2. Register the driver in Rancher

Edit [`deploy/nodedriver.yaml`](../deploy/nodedriver.yaml):

| Placeholder | Replace with |
|---|---|
| `<VERSION>` | The release tag, e.g. `v0.1.1` |
| `<SHA256>` | The sha256 of `docker-machine-driver-pve-linux-amd64` from `checksums.txt` |

The release uploads **two distinct artifacts** with very different roles —
**the `url` field must point to the binary, not the manifest**:

| Release asset | What it is | Goes where |
|---|---|---|
| `docker-machine-driver-pve-linux-amd64` | The **driver binary** (executable) | `spec.url` + `spec.checksum` of the NodeDriver |
| `docker-machine-driver-pve-linux-arm64`, `*-darwin-*` | Other arch/OS binaries | Not needed by Rancher |
| `checksums.txt` | sha256s of every binary above | Used to fill `<SHA256>` |
| `nodedriver-v<VERSION>.yaml` | The **rendered NodeDriver CRD** (this manifest, with `<VERSION>`/`<SHA256>` already substituted) | Applied with `kubectl apply -f` — **not** the `url` |

So for any release `v<x>` (≥ v0.1.2 — see the note below about why) the
correct fields are:

```yaml
spec:
  url: "https://github.com/Lore09/pve-rancher-driver/releases/download/v<x>/docker-machine-driver-pve-linux-amd64"
  checksum: "<sha256 of docker-machine-driver-pve-linux-amd64 from that release's checksums.txt>"
  whitelistDomains:
    - github.com
    - objects.githubusercontent.com
    - release-assets.githubusercontent.com
```

> ⚠️ Pointing `url` at `nodedriver-v0.1.1.yaml` (the manifest) instead of the
> binary is the **most common reason a driver sits in `Downloading`
> forever**. The mistake usually comes in two parts:
>
> - `spec.url` ends in `nodedriver-v0.1.1.yaml` instead of
>   `docker-machine-driver-pve-linux-amd64`, **and**
> - `spec.checksum` is the sha256 of the YAML (e.g. for the v0.1.1 release the
>   YAML hashes to `867c3f58…df3f3`), not of the binary
>   (`26e408680add8f06e9d5c6fe09ddc6f873601aa19a0a122c8a8037d08d255ca4`) —
>   note the v0.1.1 release binary is **not downloadable** at all because of
>   the workflow bug fixed in v0.1.2+, see the note below.
>
> Rancher downloads whatever `url` names, computes its sha256, and compares it
> to `checksum`. When the YAML matches its own checksum, the next stage
> (execute as a binary) silently never completes — `state=downloading`,
> `Downloaded=Unknown`. The fix is to point **both** fields at the binary.

Then apply the manifest to Rancher's **local** cluster:

```bash
kubectl --context rancher-local apply -f deploy/nodedriver.yaml
kubectl --context rancher-local get nodedrivers.management.cattle.io pve -w
```

Wait until `pve` reports `Active`. Status meanings:

- **`Downloading`** — Rancher is fetching the binary. If this never finishes,
  check `whitelistDomains`: it must contain `github.com`,
  `objects.githubusercontent.com` **and** `release-assets.githubusercontent.com`
  (GitHub's release redirect chain crosses all three; the manifest ships with
  all three already listed). Self-hosted binaries need their own host added.
- **`Inactive` / error** — `kubectl describe nodedriver pve` shows the cause;
  usually a checksum mismatch or an unreachable URL.

### Equivalent UI path: Add Node Driver form

**Cluster Management → Drivers → Node Drivers → Add Node Driver**, and fill
in exactly these fields:

| Field | Value |
|---|---|
| **Download URL** | `https://github.com/lore09/pve-rancher-driver/releases/download/<VERSION>/docker-machine-driver-pve-linux-amd64` (replace `<VERSION>`, e.g. `v0.1.0`) |
| **Custom Checksum** | *(toggle on)* |
| **Checksum** | The SHA-256 of `docker-machine-driver-pve-linux-amd64` from `dist/checksums.txt` |
| **Node Driver Name** | `pve` |
| **Display Name** | `Proxmox VE (pve)` |

> ⚠️ **`Whitelist Domains` is not exposed in the Add Node Driver UI form.**
> Rancher applies a default allow-list that does **not** include GitHub's
> release redirect chain. If you register the driver through the UI it will
> sit in `Downloading` forever, because GitHub redirects the request through
> `objects.githubusercontent.com` **and** `release-assets.githubusercontent.com`,
> both of which Rancher rejects without an explicit allow-list entry.
>
> **You must register via the manifest** (`kubectl apply -f
> deploy/nodedriver.yaml`) so the three `whitelistDomains` entries below take
> effect, or edit the resulting `NodeDriver` CRD by hand afterwards:
>
> ```bash
> kubectl --context rancher-local apply -f deploy/nodedriver.yaml
> # Or, if you already created it through the UI, patch the missing fields:
> kubectl --context rancher-local patch nodedriver pve --type merge -p '{
>   "spec": {
>     "whitelistDomains": ["github.com","objects.githubusercontent.com","release-assets.githubusercontent.com"]
>   }
> }'
> # And force a re-download by touching url/checksum:
> kubectl --context rancher-local patch nodedriver pve --type merge -p '{
>   "spec": { "checksum": "<SHA256>" }
> }'
> ```
>
> If you really cannot use `kubectl`, the same three domains can be added in
> the UI by editing the `pve` NodeDriver resource through the Rancher CRD
> browser / **Edit YAML** view — the Add form itself does not show the field.

> Rancher only re-downloads when `url` or `checksum` change. Every new
> release must bump **both**, or old nodes keep using the cached driver.

## 3. Create a cloud credential

The NodeDriver manifest's annotations split the driver flags into a reusable
credential and per-template options:

- **public credential fields**: `apiUrl`, `apiTokenId`
- **private credential field**: `apiTokenSecret`
- **optional**: `apiInsecure`, `caCert`

In the UI: **Cluster Management → Cloud Credentials → Create → Proxmox VE (pve)**
and fill in:

| Field | Value |
|---|---|
| `API URL` | `https://<pve-host>:8006/api2/json` |
| `API Token ID` | e.g. `rancher@pve!machine` |
| `API Token Secret` | The secret printed once by `pveum user token add` |
| `Insecure TLS` | Only for lab/self-signed certs |
| `CA Cert` | PEM content of your PVE CA (alternative to Insecure) |

When the cluster is created, Rancher stores the secret in a Kubernetes
`Secret` and hands it to the driver per machine — the secret never appears in
node-template objects.

## 4. Create a cluster with machine pools

**Cluster Management → Create → RKE2/K3s → Custom (or your cluster type) →
toggle the `Proxmox VE (pve)` node driver**, then add a machine pool. Every
driver flag shows up as a form field; the ones that matter first:

### Machine pool fields

| Field (UI) | Flag | Notes |
|---|---|---|
| Template VMID | `pve-template-vmid` | From the [template guide](template-preparation.md), e.g. `9000` |
| Node | `pve-node` | Leave empty to use the first online PVE node |
| VMID | `pve-vmid` | `0` = PVE auto-assigns (recommended) |
| Cores / Sockets / Memory | `pve-cores` / `pve-sockets` / `pve-memory` | Per-VM sizing |
| Network device | `pve-net-device` | Which PVE NIC's MAC is used for IP discovery (`net0`), and the device the settings below rewrite |
| Bridge | `pve-net-bridge` | e.g. `vmbr1`. Leave empty to inherit the template's network. See [per-pool networking](#per-pool-networking) |
| NIC model / VLAN tag / MTU / firewall | `pve-net-model` / `pve-net-vlan-tag` / `pve-net-mtu` / `pve-net-firewall` | All require **Bridge** to be set |
| Boot disk size | `pve-disk` | GB; grows the cloned boot disk. `0` = keep the template's size |
| Boot disk device | `pve-boot-disk-device` | `scsi0` by default; match your template's boot disk |
| Cloud-init | `pve-cloudinit` | Enable to push `ipconfig0`/`sshkeys`/`ciuser` |
| IP config | `pve-ipconfig` | `ip=dhcp` or static `ip=10.0.0.5/24,gw=10.0.0.1` |
| Cloud-init user | `pve-ciuser` | e.g. `rancher` for Leap Micro; leave empty for Debian's built-in `debian` user |
| Extra SSH keys | `pve-sshkeys` | Optional additional public keys (the machine's own key is always injected) |
| SSH user | `ssh-user` | **Must match the cloud-init user** — `debian` or `rancher` |
| Extra disk size | `pve-extra-disk-size` | GB; `0` = none. See [Longhorn](#longhorn--extra-data-disks) |
| Extra disk storage | `pve-extra-disk-storage` | e.g. `local-lvm`, required when size > 0 |
| Agent timeout | `pve-agent-timeout` | Seconds to wait for the guest-agent IP (default 300) |
| On boot | `pve-onboot` | Autostart VM with the PVE host |

One pool per role (control-plane, etcd, worker) is normal; workers are where
extra disks usually go.

## 5. Watch provisioning

The driver flow per node is:

```
clone template → configure (agent=1, cpu/mem/cloud-init, [network])
  → [grow boot disk] → [optional extra disk] → start
  → capture NIC MAC → poll guest agent for IPv4 (MAC-matched)
  → report created → Rancher system-agent bootstraps over SSH
```

The order is deliberate in two places. The boot disk is grown *before* the
extra disk is attached, because the extra disk lands in the first free
`scsi<N>` slot. And the NIC MAC is captured *after* the configure step,
because rewriting the net device (when `pve-net-bridge` is set) makes PVE
generate a fresh MAC — reading it earlier would pin IP discovery to the
template's old address.

Useful places to look while a node provisions:

```bash
# Rancher machine objects
kubectl --context rancher-local get machines -n fleet-default -w

# The node-driver job logs (per machine attempt)
kubectl --context rancher-local -n fleet-default logs -l app=rancher-machine --tail=100

# On the PVE side
qm list ; qm agent <vmid> ping ; qm agent <vmid> network-get-interfaces
```

Typical timing: 5–8 minutes from `+` to `Ready` on a warm template.

## Longhorn / extra data disks

- Set `pve-extra-disk-size` (e.g. `100`) and `pve-extra-disk-storage` on the
  worker pool. The driver attaches a blank SCSI disk at the first free slot
  (`scsi1` when the boot disk is `scsi0`), which the guest sees as `/dev/sdb`.
- The driver does **not** format or mount it. Bake the cloud-config drop-in
  from [template-preparation.md](template-preparation.md#optional-template-side-setup-for-longhorn-or-other-storage-provisioners)
  into the template so every clone formats/mounts it at `/var/lib/longhorn`
  on first boot.
- Only attach the extra disk to pools that actually run Longhorn storage
  (usually workers); control-plane/etcd pools should stay on the boot disk.

## Per-pool networking

By default the driver leaves the cloned VM's network exactly as the template
had it, and only *reads* the NIC's MAC address to pin down IP discovery. That
is enough when every node lives on one bridge.

Set `pve-net-bridge` on a machine pool to override it, which lets one template
serve pools on different networks instead of maintaining a template per VLAN:

| Goal | Fields |
|---|---|
| Workers on an isolated bridge | `pve-net-bridge: vmbr1` |
| Pool on VLAN 100 | `pve-net-bridge: vmbr0`, `pve-net-vlan-tag: 100` |
| Jumbo frames for a storage pool | `pve-net-bridge: vmbr0`, `pve-net-mtu: 9000` |
| Firewall the NIC | `pve-net-bridge: vmbr0`, `pve-net-firewall: true` |

Notes:

- `pve-net-vlan-tag`, `pve-net-mtu` and `pve-net-firewall` **only apply when
  `pve-net-bridge` is set**. Setting one without a bridge is rejected in
  `PreCreateCheck` rather than silently ignored, so a pool never comes up
  believing it is on a VLAN the driver never configured.
- `pve-net-firewall` is deliberately a string, not a checkbox: empty means
  "leave PVE's default alone", which a boolean cannot express. A checkbox
  defaulting to off would silently disable a firewall the template enabled.
- Rewriting the device assigns a **new MAC**. That is handled internally (the
  MAC is read after configuration), but it does mean DHCP reservations keyed to
  the template's MAC will not match.
- The driver writes only the device named by `pve-net-device`. Other NICs on
  the template are untouched.

## Boot disk sizing

`pve-disk` grows the cloned boot disk. It defaults to `0`, meaning the
template's own disk size is kept.

**PVE can only grow a disk, never shrink it.** A value smaller than the
template's disk is rejected by the PVE API, so leave the field at `0` rather
than trying to reduce it. If your template does not boot from `scsi0`, set
`pve-boot-disk-device` to match (`virtio0`, `sata0`, ...) — the resize targets
that key by name and will fail if the device does not exist.

Note this grows the *block device*. Whether the guest's filesystem expands to
fill it depends on the image: most cloud images run `growpart`/`cloud-initramfs`
on first boot and will. Verify with `lsblk` and `df -h` on a test node before
relying on it for a whole pool.

## Fixing a driver already stuck in `Downloading`

If you already registered the driver through the UI form (or applied the
manifest with the wrong URL), patch it in place instead of deleting it:

```bash
# 1. Get the binary's sha256 from the release (use a release >= v0.1.2).
#    v0.1.1 shipped no binaries at all — see the note below.
RELEASE=v0.1.2   # or whatever the latest tag is
curl -sL "https://github.com/Lore09/pve-rancher-driver/releases/download/${RELEASE}/checksums.txt" \
  | grep docker-machine-driver-pve-linux-amd64
#   -> <sha256>  docker-machine-driver-pve-linux-amd64

# 2. Patch all three wrong fields in one shot
kubectl --context rancher-local patch nodedriver pve --type merge -p '{
  "spec": {
    "url": "https://github.com/Lore09/pve-rancher-driver/releases/download/'"${RELEASE}"'/docker-machine-driver-pve-linux-amd64",
    "checksum": "<sha256-from-step-1>",
    "whitelistDomains": [
      "github.com",
      "objects.githubusercontent.com",
      "release-assets.githubusercontent.com"
    ]
  }
}'

# 3. Force Rancher to re-fetch by changing url/checksum (already done above);
#    then watch it flip from Downloading -> Active
kubectl --context rancher-local get nodedrivers.management.cattle.io pve -w
```

The matching `checksum` always comes from the **same release's** `checksums.txt`
for the arch matching `url` — for a Rancher server running on linux/amd64 that
is the `docker-machine-driver-pve-linux-amd64` line. Do **not** paste the
sha256 of the `nodedriver-v*.yaml` file (it's a manifest, not the binary).

> ℹ️ **Release `v0.1.1` is broken.** Its GitHub release contains only
> `checksums.txt` and `nodedriver-v0.1.1.yaml` — **no driver binaries** —
> because the release workflow used `goreleaser archives.formats: [binary]`,
> which writes binaries into subdirs (`dist/docker-machine-driver-pve_<os>_<arch>_vX/`)
> without creating the flat archive files that `checksums.txt` references.
> The upload glob `dist/docker-machine-driver-pve-*` matched the subdirs,
> which `softprops/action-gh-release` silently skips. Use a release **≥ v0.1.2**
> — the workflow now flattens the binaries before upload, so the
> `docker-machine-driver-pve-linux-amd64` asset actually exists.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Driver stuck in `Downloading`, `Downloaded=Unknown` | None of the three: (a) `url` points to the YAML manifest not the binary, (b) missing `whitelistDomains` entry, (c) `checksum` mismatch vs. `url` | Verify `spec.url` ends in `docker-machine-driver-pve-linux-amd64`; recompute sha256 of that exact asset; ensure all three GitHub redirect hosts are listed in `whitelistDomains`; re-apply |
| Driver stuck in `Downloading` but only after fixing `url` | Missing `whitelistDomains` entry — GitHub redirects through `objects.githubusercontent.com` and `release-assets.githubusercontent.com` | Both must be added; `github.com` alone is not enough |
| "API token is missing privileges" at save | Token ACL not granted to the token itself | Run both `pveum acl modify` lines (user **and** `-token`) from the README |
| Node template dropdowns empty / clones fail silently | Same as above — token has zero effective ACLs (privsep) | Same fix; or `--pve-skip-permission-check` to bypass the probe |
| Create times out "waiting for guest agent IP" | **qemu-guest-agent not installed or not running inside the image.** The driver now sets `agent=1` on every clone, so the PVE-side channel is no longer a cause | Re-bake the image with `qemu-guest-agent` installed and enabled; verify with `qm agent <id> ping` |
| VM boots but node never `Ready` | cloud-init user lacks passwordless sudo, or `curl`/`bash` missing | Fix template; verify `sudo -n true` works for the SSH user |
| SSH permission denied during bootstrap | `ssh-user` doesn't match the cloud-init user, or keys not injected | Match users; keep `pve-cloudinit` on; check the VM's cloud-init log (`/var/log/cloud-init-output.log`) |
| Extra disk missing in guest | `pve-extra-disk-storage` wrong/empty | Field is required when size > 0; check storage name with `pvesm status` |
| IP picked from wrong interface (docker0/cni) | First-IPv4 fallback was used | Ensure MAC capture succeeded (driver logs); set `pve-net-device` to the right `netN` |
| "require --pve-net-bridge to be set" at save | A VLAN tag / MTU / firewall value was set without a bridge — those only apply while rewriting the net device | Set `pve-net-bridge`, or clear the other `pve-net-*` fields. See [per-pool networking](#per-pool-networking) |
| Boot disk still the template's size | `pve-disk` left at `0`, or the guest did not grow its filesystem | Set `pve-disk`; check `lsblk` vs `df -h` in the guest — the block device grows, the filesystem only follows if the image runs `growpart` |
| Resize fails "disk ... does not exist" | Template does not boot from `scsi0` | Set `pve-boot-disk-device` to the template's actual boot disk key (`qm config <vmid>`) |
| Node gets an unexpected IP after setting a bridge | Rewriting the net device assigns a new MAC, so DHCP reservations keyed to the old MAC no longer match | Re-key the reservation to the new MAC, or use `pve-ipconfig` for a static address |

## Upgrading the driver

1. Build/publish the new release.
2. Edit `deploy/nodedriver.yaml` with the new `<VERSION>` **and** `<SHA256>`.
3. `kubectl apply` — Rancher detects the changed URL/checksum and
   re-downloads; existing clusters are untouched until a node is
   (re)provisioned.
