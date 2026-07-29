# Installation reference

The [README](../README.md) covers the normal path: add two repositories in
Rancher, install two charts. This page is the reference for everything else —
what the chart actually does, the alternative install methods, self-hosting the
binary, and upgrades.

## What gets installed

| Component | Repo | Chart | Installs into |
|---|---|---|---|
| Node driver | `pve-rancher-driver` | `pve-rancher-driver` | `cattle-system` on the **local** cluster |
| UI extension | `pve-rancher-ui-extension` | `pve` | `cattle-ui-plugin-system` |

The driver chart creates one cluster-scoped `NodeDriver` resource. That resource
tells Rancher where to download the driver binary, its SHA-256, and which hosts
Rancher may reach on its behalf. The UI extension is optional in the strict
sense — without it Rancher derives a generic form from the driver's flags — but
the polished machine-pool form, the storage/template/bridge dropdowns and
**Test Connection** all come from it.

Both belong on the Rancher **local** (management) cluster. A `NodeDriver` is a
`management.cattle.io/v3` resource that means nothing on a downstream cluster;
installing it there appears to succeed and does nothing. The chart guards
against that with `verifyManagementCluster`, so the mistake fails loudly.

## The allow-list is the part people get wrong

`nodeDriver.whitelistDomains` gates two unrelated things, and an incomplete list
produces two very different symptoms:

1. **Downloading the binary.** GitHub's release redirect chain crosses
   `github.com`, `objects.githubusercontent.com` and
   `release-assets.githubusercontent.com`. Drop any one and the driver sits in
   `Downloading` forever with no error.
2. **The UI extension's PVE calls.** Test Connection and the
   template/storage/bridge dropdowns go through Rancher's `/meta/proxy`, which
   is gated by the same list. **Add your PVE hostname** — hostname only, no
   scheme and no `:8006`, because Rancher matches the URL's hostname:

```yaml
nodeDriver:
  whitelistDomains:
    - github.com
    - objects.githubusercontent.com
    - release-assets.githubusercontent.com
    - pve.example.com      # <- yours
```

Provisioning itself does **not** use that proxy, so a missing PVE host breaks
the dropdowns while leaving provisioning working — which is why the extension
degrades to plain text inputs instead of blocking.

That proxy also verifies PVE's TLS certificate against the Rancher server's
trust store, which a stock self-signed PVE certificate fails. The credential's
`Insecure TLS` / `CA Cert` fields apply to the **driver only**, not the proxy.
Either [add the PVE CA to Rancher](rancher-setup.md#make-rancher-trust-the-proxmox-ve-certificate)
or accept the warning and type the machine-pool fields by hand.

## Chart values

The full annotated list lives in [`deploy/chart/values.yaml`](../deploy/chart/values.yaml)
and the chart's own [README](../deploy/chart/README.md). The ones worth knowing:

| Value | Default | When you change it |
|---|---|---|
| `nodeDriver.arch` | `linux-amd64` | Your **Rancher server** runs on arm64. This is the architecture of the management server, not of the VMs being provisioned |
| `nodeDriver.whitelistDomains` | GitHub hosts | Always — to add your PVE host. See above |
| `nodeDriver.version` | `""` (chart's appVersion) | Pinning an older binary against a newer chart |
| `nodeDriver.checksum` | written by CI | Never by hand — it is generated after the binary is built |
| `release.baseUrl` | GitHub releases | Self-hosting or mirroring the binary |
| `verifyManagementCluster` | `true` | Only for offline `helm template` rendering |
| `retainOnUninstall` | `true` | You genuinely want `helm uninstall` to remove the driver. Removing a driver that existing clusters provision against breaks their node scale-up |

## Alternative: install the chart with Helm

```bash
helm install pve-rancher-driver \
  https://github.com/Lore09/pve-rancher-driver/releases/download/v<version>/pve-rancher-driver-<version>.tgz \
  -n cattle-system \
  --set-json 'nodeDriver.whitelistDomains=["github.com","objects.githubusercontent.com","release-assets.githubusercontent.com","pve.example.com"]'
```

Or from a checkout:

```bash
helm install pve-rancher-driver deploy/chart -n cattle-system
```

## Alternative: the raw NodeDriver manifest

For a cluster where you would rather not run Helm at all:

```bash
kubectl apply -f deploy/nodedriver.yaml
```

Edit it first and replace `<VERSION>` and `<SHA256>` with the matching line from
the release's `checksums.txt`, for the OS/arch of the host running the Rancher
management plane. The `whitelistDomains` caveats above apply identically.

## Alternative: Add Node Driver in the UI

**Cluster Management → Drivers → Node Drivers → Add Node Driver**, pasting the
binary URL and checksum by hand.

This is the least good option and it is worth saying why: that form does not
expose `whitelistDomains`, and Rancher's default allow-list does not include
GitHub's release redirect chain — so the download stalls and the form gives you
no way to fix it. Use it only if you are self-hosting the binary on a host
Rancher already trusts.

## Self-hosting the binary (air-gapped)

Mirror the release asset to a host your Rancher server can reach, then:

```yaml
release:
  baseUrl: https://artifacts.internal/pve-driver
nodeDriver:
  whitelistDomains:
    - artifacts.internal
    - pve.example.com
```

The binary name and the checksum still have to match — the chart builds the URL
as `<baseUrl>/v<version>/<binaryName>-<arch>`.

## Upgrading

Upgrade the driver chart and the extension chart independently; there is no
version coupling between them. Rancher caches the machine-config schema per
driver version, so **new driver flags do not appear in the form until the
driver chart is upgraded** — if you upgrade only the extension, new fields will
be missing and the form may fail to bind them.

Existing machine pools keep using the driver version they were created with
until they are edited.

## Uninstalling

```bash
helm uninstall pve-rancher-driver -n cattle-system
```

The `NodeDriver` is annotated `helm.sh/resource-policy: keep`, so it survives by
design — deleting a driver that existing clusters still provision against breaks
node scale-up on those clusters. Set `retainOnUninstall: false` before
uninstalling if you really want it gone, or delete it by hand afterwards:

```bash
kubectl delete nodedriver pve
```
