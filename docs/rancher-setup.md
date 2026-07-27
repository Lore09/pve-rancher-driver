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
| `<VERSION>` | The release tag, e.g. `v0.1.0` |
| `<SHA256>` | The sha256 of `docker-machine-driver-pve-linux-amd64` from `checksums.txt` |

Then apply it to Rancher's **local** cluster:

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

Equivalent UI path: **Cluster Management → Drivers → Node Drivers → Add Node
Driver**, paste the same URL and checksum.

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
| Network device | `pve-net-device` | Which PVE NIC's MAC is used for IP discovery (`net0`) |
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
clone template → configure (cpu/mem/cloud-init) → [optional extra disk]
  → start → capture NIC MAC → poll guest agent for IPv4 (MAC-matched)
  → report created → Rancher system-agent bootstraps over SSH
```

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

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Driver stuck in `Downloading` | Missing `whitelistDomains` entry | Ensure all three GitHub redirect hosts are listed; re-apply |
| "API token is missing privileges" at save | Token ACL not granted to the token itself | Run both `pveum acl modify` lines (user **and** `-token`) from the README |
| Node template dropdowns empty / clones fail silently | Same as above — token has zero effective ACLs (privsep) | Same fix; or `--pve-skip-permission-check` to bypass the probe |
| Create times out "waiting for guest agent IP" | qemu-guest-agent missing/disabled in image, or agent enabled but `--agent 1` missing on template | Re-bake image, set `--agent 1`, verify with `qm agent <id> ping` |
| VM boots but node never `Ready` | cloud-init user lacks passwordless sudo, or `curl`/`bash` missing | Fix template; verify `sudo -n true` works for the SSH user |
| SSH permission denied during bootstrap | `ssh-user` doesn't match the cloud-init user, or keys not injected | Match users; keep `pve-cloudinit` on; check the VM's cloud-init log (`/var/log/cloud-init-output.log`) |
| Extra disk missing in guest | `pve-extra-disk-storage` wrong/empty | Field is required when size > 0; check storage name with `pvesm status` |
| IP picked from wrong interface (docker0/cni) | First-IPv4 fallback was used | Ensure MAC capture succeeded (driver logs); set `pve-net-device` to the right `netN` |

## Upgrading the driver

1. Build/publish the new release.
2. Edit `deploy/nodedriver.yaml` with the new `<VERSION>` **and** `<SHA256>`.
3. `kubectl apply` — Rancher detects the changed URL/checksum and
   re-downloads; existing clusters are untouched until a node is
   (re)provisioned.
