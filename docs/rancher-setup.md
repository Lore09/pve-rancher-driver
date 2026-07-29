# Integrating the `pve` node driver with Rancher

This walks the whole path from a built driver binary to a working RKE2/K3s
cluster whose nodes are Proxmox VE VMs. It assumes:

- Rancher **v2.x** (management.cattle.io/v3 NodeDriver support) and `kubectl`
  access to the **local** (management) cluster.
- A Proxmox VE cluster reachable from the Rancher pods, with the API token
  from the [README](../README.md#prepare-proxmox-ve) and a template from
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

There are two supported paths. **Option A needs no `kubectl` and is
recommended**; Option B is the explicit route for GitOps and for debugging.

### Option A: add this repo as a Rancher Repository (recommended)

Rancher can serve Helm charts straight out of a git repo — it clones the
repository and, finding no `index.yaml`, discovers and packages chart
directories itself. So there is nothing to publish: point Rancher at this
repository and the chart appears.

1. In the **local** cluster: **Apps → Repositories → Create**
2. Choose **Git repository**, then:

   | Field | Value |
   |---|---|
   | Name | `pve-rancher-driver` |
   | Git Repo URL | `https://github.com/lore09/pve-rancher-driver.git` |
   | Git Branch | `master` |

3. **Apps → Charts**, pick **Proxmox VE Node Driver**, and install into the
   `local` cluster. The namespace is pre-filled as **`cattle-system`** by the
   chart; accept it. For an arm64 Rancher server, set `nodeDriver.arch` to
   `linux-arm64` first.

   The namespace is nearly arbitrary — a `NodeDriver` is cluster scoped, so the
   namespace holds only Helm's release metadata — but `cattle-system` always
   exists on the local cluster and will not be deleted out from under it.

Why this is the recommended path: the chart sets `whitelistDomains`, which the
**Add Node Driver** form does not expose (see the warning further down). That
single omission is the most common cause of a driver stuck in `Downloading`
forever, and the chart removes it entirely.

Things worth knowing:

- **The chart must go into the `local` cluster.** A `NodeDriver` is cluster
  scoped and only meaningful on the management cluster. The chart refuses to
  install where `management.cattle.io/v3` is not served, so a downstream
  install fails loudly rather than silently doing nothing.
- **Repositories refresh hourly by default** (`spec.refreshInterval`). A newly
  released version will not appear immediately; use **Refresh** on the
  repository to force it.
- **`helm uninstall` leaves the driver behind** on purpose
  (`helm.sh/resource-policy: keep`), so uninstalling the app cannot break node
  scale-up on clusters still using it. See the
  [chart README](../deploy/chart/README.md#uninstalling) to remove it fully.
- **Never commit an `index.yaml` to this repo.** Rancher uses the shallowest
  `index.yaml` it finds anywhere in the clone *instead of* discovering charts,
  so a stray one would pin the catalog to stale versions. A symlink anywhere in
  the repo is also fatal — Rancher rejects the whole repository.

Chart values are documented in the [chart README](../deploy/chart/README.md),
including air-gapped installs that mirror the binary internally.

### Option B: apply the NodeDriver manifest with kubectl

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

So for any release later than `v0.1.1` (see the note below about why that one
is unusable) the correct fields are:

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
>   the workflow bug fixed after v0.1.1, see the note below.
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
> **Use the Helm chart ([Option A](#option-a-add-this-repo-as-a-rancher-repository-recommended))
> or the manifest** — both set the three `whitelistDomains` entries, and the
> chart does it without needing `kubectl`. If you already created the driver
> through the Add form, patch the missing fields by hand:
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
| `Insecure TLS` | Only for lab/self-signed certs. Used by the **driver** when it provisions; it does not affect the UI's Test Connection — see [below](#make-rancher-trust-the-proxmox-ve-certificate) |
| `CA Cert` | PEM content of your PVE CA (alternative to Insecure). Same scope as above: driver only |

When the cluster is created, Rancher stores the secret in a Kubernetes
`Secret` and hands it to the driver per machine — the secret never appears in
node-template objects.

### Allow-list the PVE host first

**Test Connection** (and the template/storage/bridge dropdowns in the machine
pool form) run in your browser and reach PVE through Rancher's `/meta/proxy`,
which refuses any host that is not in the driver's `whitelistDomains`. Add the
PVE **hostname** — no scheme, no `:8006`, because Rancher matches the URL's
hostname:

```bash
kubectl patch nodedriver.management.cattle.io pve --type=json \
  -p '[{"op":"add","path":"/spec/whitelistDomains/-","value":"<pve-host>"}]'
```

Or set it at install time by passing the whole list (Helm's
`--set …whitelistDomains[3]=x` replaces the list rather than appending, blanking
the GitHub entries):

```bash
helm upgrade pve-rancher-driver deploy/chart -n cattle-system --reuse-values \
  --set 'nodeDriver.whitelistDomains={github.com,objects.githubusercontent.com,release-assets.githubusercontent.com,<pve-host>}'
```

If you patch the live resource instead, add the host to your Helm values too or
the next upgrade reverts it. The driver binary does not use this proxy, so a
driver that reached `Active` can still fail here.

### Make Rancher trust the Proxmox VE certificate

Allow-listing the host gets the proxy as far as *connecting* to PVE. It then
validates the certificate against the **Rancher server's** trust store, and
there is no way to turn that off: Rancher's proxy uses Go's default HTTP
transport with no TLS overrides. `Insecure TLS` and `CA Cert` on the credential
are read by the **driver**, which connects to PVE directly and never goes
through the proxy — they have no effect here.

A stock PVE install serves port 8006 with a certificate signed by its own
cluster CA, which nothing trusts by default, so this step applies to most
self-hosted setups.

**Confirm this is your problem.** Run the same request from inside the Rancher
pod:

```bash
kubectl -n cattle-system exec deploy/rancher -- \
  curl -sS -o /dev/null -w '%{http_code}\n' https://<pve-host>:8006/api2/json/version
```

| Output | Meaning |
|---|---|
| `curl: (60) ... unable to get local issuer certificate` (or `self signed certificate`) | Trust problem — continue below |
| `Could not resolve host` | In-cluster DNS cannot resolve the PVE hostname (common with `.home`/`.lan` domains served only by your router). Use a resolvable name or an IP, and allow-list whatever you use |
| `Connection refused` / a timeout | Network or firewall between the Rancher pod and PVE |
| `401` | TLS and connectivity are fine — the trust step is already done |

**1. Get the PVE cluster CA.** On a default install it is
`/etc/pve/pve-root-ca.pem`, shared by every node in the cluster and unchanged
when individual node certificates are renewed:

```bash
scp root@<pve-host>:/etc/pve/pve-root-ca.pem .
```

If you replaced PVE's certificate with your own, use *that* chain's CA instead.
For a self-signed certificate with no separate CA, export the served
certificate itself:

```bash
openssl s_client -connect <pve-host>:8006 -showcerts </dev/null 2>/dev/null \
  | openssl x509 -out pve-root-ca.pem
```

**2. Give it to Rancher.** For a Helm-installed Rancher (the normal case):

```bash
kubectl -n cattle-system create secret generic tls-ca-additional \
  --from-file=ca-additional.pem=pve-root-ca.pem

helm upgrade rancher rancher-stable/rancher -n cattle-system --reuse-values \
  --set additionalTrustedCAs=true

kubectl -n cattle-system rollout restart deploy/rancher
```

Use whichever chart repo you installed from — `rancher-latest`, `rancher-stable`
or `rancher-prime`; `helm -n cattle-system get metadata rancher` will tell you.

Three things go wrong here more often than anything else:

- **The key name must be `ca-additional.pem`.** Rancher's entrypoint looks for
  exactly that filename and ignores the secret otherwise.
- **`additionalTrustedCAs=true` is what mounts the secret** and runs
  `update-ca-certificates` so the CA lands in the system pool Go reads. Creating
  the secret alone does nothing.
- **If `tls-ca-additional` already exists**, do not replace it — concatenate the
  existing PEM and the PVE CA into one file and recreate the secret from that,
  or you will drop whatever CA was already trusted.

  ```bash
  kubectl -n cattle-system get secret tls-ca-additional \
    -o jsonpath='{.data.ca-additional\.pem}' | base64 -d > existing.pem
  cat existing.pem pve-root-ca.pem > combined.pem
  kubectl -n cattle-system create secret generic tls-ca-additional \
    --from-file=ca-additional.pem=combined.pem --dry-run=client -o yaml \
    | kubectl apply -f -
  ```

For a Docker-install Rancher, bind-mount the same file instead and restart the
container:

```bash
-v /path/to/pve-root-ca.pem:/etc/rancher/ssl/ca-additional.pem
```

**3. Verify.** Re-run the `curl` from the top of this section — it should print
`401` (reachable, TLS verified, token not passed) instead of `curl: (60)`. Then
reload the Rancher UI tab and press **Test Connection** again.

### If you would rather not touch Rancher's trust store

Nothing about provisioning depends on this proxy, so you can skip the whole
thing:

- **Test Connection** reports a *warning* rather than an error when the host is
  allow-listed but unreachable, and the cloud credential saves anyway.
- The machine pool form detects the same condition and replaces the four
  discovered dropdowns — node, template VMID, extra-disk storage, bridge — with
  plain text inputs. Fill them in from the PVE UI (node name under
  *Datacenter*, template VMID from the [template
  guide](template-preparation.md), storage from *Datacenter → Storage*, bridge
  from *Node → Network*).
- Clones then run through the driver inside the Rancher pod, which honours
  `Insecure TLS` / `CA Cert` — so a self-signed PVE certificate is fine there.

The only thing you lose is discovery in the form. If typing a wrong node name or
VMID worries you, do the trust step instead.

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
| Boot disk size | `pve-boot-disk-size` | GB; grows the cloned boot disk. `0` = keep the template's size |
| Boot disk device | `pve-boot-disk-device` | `scsi0` by default; match your template's boot disk |
| Cloud-init | `pve-cloudinit` | Enable to push `ipconfig0`/`sshkeys`/`ciuser` |
| IP config | `pve-ipconfig` | `ip=dhcp` or static `ip=10.0.0.5/24,gw=10.0.0.1` |
| Cloud-init user | `pve-ciuser` | e.g. `rancher` for Leap Micro; leave empty for Debian's built-in `debian` user |
| Extra SSH keys | `pve-sshkeys` | Optional additional public keys (the machine's own key is always injected) |
| SSH user | `ssh-user` | **Must match the cloud-init user** — `debian` or `rancher`. Defaults to `root`, which neither documented template permits: leave it at the default and the node provisions and then never reaches `Ready` |
| VM name prefix | `pve-vmname-prefix` | Optional. Rendered as `<prefix>-<machine name>`, e.g. `k8s-mycluster-pool1-x7k2p`. Empty uses the machine name unchanged. Letters, digits and inner hyphens only |
| Data Disks | `pve-data-disk` | One row per disk; repeatable. See [Data disks](#data-disks) below |
| Agent timeout | `pve-agent-timeout` | Seconds to wait for the guest-agent IP (default 300) |
| On boot | `pve-onboot` | Autostart VM with the PVE host |

One pool per role (control-plane, etcd, worker) is normal; workers are where
data disks usually go.

### Data disks

The **Data Disks** section takes one row per disk. Add as many as the pool
needs:

| Column | Meaning |
|---|---|
| Size (GB) | Required, must be greater than 0 |
| Storage | PVE storage id, picked from the dropdown (or typed if the API is unreachable) |
| Filesystem | `ext4`, `xfs`, or `none` |
| Mount Path | Absolute path, e.g. `/var/lib/longhorn`. Required unless the filesystem is `none`, which attaches the disk raw and leaves the guest alone |
| Include in PVE backups | Off by default — a replicated Longhorn volume gains nothing from a host-level backup |

Unless the filesystem is `none`, the driver formats the disk and mounts it
before the node joins the cluster, so **Cloud-init must be enabled** on the pool
— that is how the driver's SSH key reaches the guest. A pool that asks for a
filesystem with cloud-init off is rejected before any VM is cloned.

Mount paths may only contain `A-Z a-z 0-9 . _ / -`; the driver refuses anything
else, since the path is interpolated into the setup script it runs as root.

## 5. Watch provisioning

The driver flow per node is:

```
clone template → configure (agent=1, cpu/mem/cloud-init, [network])
  → [grow boot disk] → [attach data disks] → start
  → capture NIC MAC → poll guest agent for IPv4 (MAC-matched)
  → [SSH in, format and mount the data disks]
  → report created → Rancher system-agent bootstraps over SSH
```

The order is deliberate in three places. The boot disk is grown *before* the
data disks are attached, because slot allocation scans the live config for free
`scsi<N>` keys. The NIC MAC is captured *after* the configure step, because
rewriting the net device (when `pve-net-bridge` is set) makes PVE generate a
fresh MAC — reading it earlier would pin IP discovery to the template's old
address. And the disks are mounted *before* `Create` returns, so a node can
never join the cluster with its storage directory unmounted — the storage
provisioner would otherwise start filling the root filesystem.

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

## Longhorn / data disks

- Add a Data Disks row on the worker pool: size `100`, your storage, `ext4`,
  mount `/var/lib/longhorn`. The driver attaches it at the first free SCSI slot,
  stamps the disk's serial so the guest can find it regardless of `sd*`
  ordering, then formats and mounts it before the node joins.
- Bake Longhorn's guest packages into the template first — `open-iscsi` with
  `iscsid` enabled, `nfs-common`, and the multipath blacklist. See
  [template-preparation.md](template-preparation.md#guest-dependencies-for-longhorn).
  The driver mounts disks but never installs packages.
- Only give data disks to pools that actually run Longhorn storage (usually
  workers); control-plane/etcd pools should stay on the boot disk.
- There is no cloud-init disk drop-in any more. If an older template still has
  one, remove it: two mechanisms formatting the same device can lose data.

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

`pve-boot-disk-size` grows the cloned boot disk. It defaults to `0`, meaning the
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
# 1. Get the binary's sha256 from the release. Any release later than v0.1.1
#    works; v0.1.1 shipped no binaries at all — see the note below.
RELEASE="$(gh release view --json tagName -q .tagName -R Lore09/pve-rancher-driver)"
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
> which `softprops/action-gh-release` silently skips. **Use any release later
> than `v0.1.1`** — the workflow now flattens the binaries before upload, so the
> `docker-machine-driver-pve-linux-amd64` asset actually exists.
>
> Note that `v0.1.2` does not exist either: the flatten fix was committed but
> never tagged, so `v0.1.1` remained the newest published release until the
> chart-driven release process landed. Always resolve the latest tag rather than
> hardcoding a version.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Driver stuck in `Downloading`, `Downloaded=Unknown` | None of the three: (a) `url` points to the YAML manifest not the binary, (b) missing `whitelistDomains` entry, (c) `checksum` mismatch vs. `url` | Verify `spec.url` ends in `docker-machine-driver-pve-linux-amd64`; recompute sha256 of that exact asset; ensure all three GitHub redirect hosts are listed in `whitelistDomains`; re-apply |
| Driver stuck in `Downloading` but only after fixing `url` | Missing `whitelistDomains` entry — GitHub redirects through `objects.githubusercontent.com` and `release-assets.githubusercontent.com` | Both must be added; `github.com` alone is not enough |
| "Rancher could not reach the Proxmox VE server — the host is not in the node driver allow list" at Test Connection | PVE host missing from `spec.whitelistDomains`, which gates Rancher's `/meta/proxy` | Add the PVE hostname (no scheme, no port) — see [Allow-list the PVE host first](#allow-list-the-pve-host-first) |
| Test Connection warns that Rancher could not reach or verify the host, once allow-listed | Rancher server does not trust the PVE TLS certificate; the proxy always verifies it and ignores the credential's `Insecure TLS` / `CA Cert`. Confirm with the in-pod `curl` | [Make Rancher trust the PVE CA](#make-rancher-trust-the-proxmox-ve-certificate), or accept the warning and type the machine-pool fields by hand — provisioning does not use this proxy |
| Machine pool dropdowns are text inputs instead of dropdowns | Expected fallback when the PVE API is unreachable through the proxy — usually the certificate trust issue above | Fill the four fields by hand, or fix trust to get discovery back |
| Test Connection says the credentials are not allowed / unauthorized | Wrong `API Token ID` or secret (`/version` needs no privileges, so this is not an ACL problem). Before the fix in the UI extension, the token was sent in `Authorization`, which Rancher itself rejected with 401 | Re-check the token id is `user@realm!tokenid` and the secret is the one printed by `pveum user token add`; update the UI extension |
| "API token is missing privileges" at save | Token ACL not granted to the token itself | Run both `pveum acl modify` lines (user **and** `-token`) from the README |
| Node template dropdowns empty / clones fail silently | Same as above — token has zero effective ACLs (privsep) | Same fix; or `--pve-skip-permission-check` to bypass the probe |
| Create times out "waiting for guest agent IP" | **qemu-guest-agent not installed or not running inside the image.** The driver now sets `agent=1` on every clone, so the PVE-side channel is no longer a cause | Re-bake the image with `qemu-guest-agent` installed and enabled; verify with `qm agent <id> ping` |
| VM boots but node never `Ready` | cloud-init user lacks passwordless sudo, or `curl`/`bash` missing | Fix template; verify `sudo -n true` works for the SSH user |
| SSH permission denied during bootstrap | `ssh-user` doesn't match the cloud-init user, or keys not injected | Match users; keep `pve-cloudinit` on; check the VM's cloud-init log (`/var/log/cloud-init-output.log`) |
| Data disk missing in guest | wrong storage id on the row | Check the name with `pvesm status` |
| `Create` fails with `data disk setup failed` | the guest rejected the setup script — usually `mkfs.xfs` missing, or `sudo` prompting for a password | Read the guest output in the error, then fix the template (see the [dependency matrix](template-preparation.md#guest-dependencies-for-longhorn)) |
| `Create` fails with `no block device with serial pvedata1` | the guest cannot see disk serials, e.g. an unusual SCSI controller in the template | Use `virtio-scsi-single` as in the template guide; verify with `lsblk -ndo NAME,SERIAL` |
| `Create` fails with `--pve-cloudinit is required to format and mount` | a data disk asks for a filesystem but cloud-init is off, so no SSH key reaches the guest | Enable cloud-init on the pool, or set the row's filesystem to `none` |
| `Create` fails with `data disk setup did not finish within` | mkfs on a very large disk, or a slow first boot | Raise `pve-disk-setup-timeout` |
| IP picked from wrong interface (docker0/cni) | First-IPv4 fallback was used | Ensure MAC capture succeeded (driver logs); set `pve-net-device` to the right `netN` |
| "require --pve-net-bridge to be set" at save | A VLAN tag / MTU / firewall value was set without a bridge — those only apply while rewriting the net device | Set `pve-net-bridge`, or clear the other `pve-net-*` fields. See [per-pool networking](#per-pool-networking) |
| Boot disk still the template's size | `pve-boot-disk-size` left at `0`, or the guest did not grow its filesystem | Set `pve-boot-disk-size`; check `lsblk` vs `df -h` in the guest — the block device grows, the filesystem only follows if the image runs `growpart` |
| Resize fails "disk ... does not exist" | Template does not boot from `scsi0` | Set `pve-boot-disk-device` to the template's actual boot disk key (`qm config <vmid>`) |
| Node gets an unexpected IP after setting a bridge | Rewriting the net device assigns a new MAC, so DHCP reservations keyed to the old MAC no longer match | Re-key the reservation to the new MAC, or use `pve-ipconfig` for a static address |

## Upgrading the driver

1. Build/publish the new release.
2. Edit `deploy/nodedriver.yaml` with the new `<VERSION>` **and** `<SHA256>`.
3. `kubectl apply` — Rancher detects the changed URL/checksum and
   re-downloads; existing clusters are untouched until a node is
   (re)provisioned.
