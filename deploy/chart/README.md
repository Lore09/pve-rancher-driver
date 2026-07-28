# Proxmox VE Node Driver

Registers the `pve` node driver with Rancher so RKE2/K3s machine pools can
provision Proxmox VE virtual machines cloned from a template.

## Which namespace?

**`cattle-system`, in the `local` cluster.** The chart declares this via
`catalog.cattle.io/namespace`, so Rancher's Apps UI pre-fills it and you should
not need to choose.

The namespace barely matters, and it is worth knowing why: a `NodeDriver` is a
**cluster-scoped** resource, so nothing in this chart is actually namespaced.
The namespace holds only the Helm release metadata secret
(`sh.helm.release.v1.pve-rancher-driver.v1`). `cattle-system` is the right home
for it because it always exists on a Rancher local cluster — no
`--create-namespace` needed — and is not something anyone deletes casually.

Deleting the namespace would orphan the Helm release (the driver itself would
survive, thanks to `helm.sh/resource-policy: keep`, but Helm would lose track of
it and you would have to re-install to manage it again).

Via CLI:

```bash
helm install pve-rancher-driver deploy/chart -n cattle-system
```

## Install this into the `local` cluster

A `NodeDriver` is a cluster-scoped `management.cattle.io/v3` resource that only
means anything on the Rancher **local** (management) cluster. Installing this
chart into a downstream cluster appears to succeed and does nothing — the chart
refuses to install where `management.cattle.io/v3` is not served, so that
mistake fails loudly instead.

## Why use the chart instead of the manifest

Rancher's **Add Node Driver** form does not expose `whitelistDomains`, and
Rancher's default allow-list does not include GitHub's release redirect chain.
Registering the driver through that form leaves it stuck in `Downloading`
forever. This chart sets all three required hosts, so the driver installs
correctly without needing `kubectl`.

## Configuration

| Value | Default | Notes |
|---|---|---|
| `nodeDriver.arch` | `linux-amd64` | Architecture of the **Rancher server**, not the provisioned nodes. Use `linux-arm64` for an arm64 Rancher |
| `nodeDriver.version` | chart `appVersion` | Override only to pin an older binary against a newer chart |
| `nodeDriver.checksum` | *(written by CI)* | SHA-256 of the binary. Do not edit by hand |
| `nodeDriver.checksumFor` | *(written by CI)* | Version the checksum belongs to; a mismatch blocks rendering |
| `nodeDriver.whitelistDomains` | 3 GitHub hosts | Replace the GitHub entries when self-hosting the binary. **Append your PVE hostname** (no port, no scheme) so the UI extension's Test Connection and dropdowns can reach the PVE API through Rancher's proxy |
| `nodeDriver.active` | `true` | Whether the driver is enabled |
| `nodeDriver.uiUrl` | `""` | UI extension bundle. Empty means a generic icon and a form derived from the driver's flags |
| `release.baseUrl` | GitHub releases | Point at an internal mirror for air-gapped installs |
| `verifyManagementCluster` | `true` | Set `false` only for offline `helm template` |
| `retainOnUninstall` | `true` | Keeps the NodeDriver on `helm uninstall` |

## Upgrading

Rancher only re-downloads the driver binary when `url` or `checksum` change.
Both are derived from the chart's `appVersion`, so a chart upgrade always
changes them together — there is no way to bump one without the other.

Existing clusters keep running the cached driver until a node is
(re)provisioned; upgrading does not disturb running nodes.

## Uninstalling

By default `helm uninstall` leaves the `NodeDriver` in place
(`helm.sh/resource-policy: keep`), because deleting a driver that existing
clusters still provision against breaks their node scale-up. To remove it
deliberately:

```bash
helm uninstall pve-rancher-driver -n cattle-system
kubectl delete nodedriver.management.cattle.io pve
```

### Re-installing after an uninstall

Because the uninstall deliberately leaves the `NodeDriver` behind, a plain
re-install fails: Helm finds a resource it does not own and refuses to adopt it.

```
Error: ... NodeDriver "pve" in namespace "" exists and cannot be imported
into the current release: invalid ownership metadata
```

Either delete the leftover driver first (the `kubectl delete` above), or adopt
it:

```bash
helm install pve-rancher-driver deploy/chart -n cattle-system --take-ownership
```

Adopting is the safer option on a live system — it leaves the driver in place,
so clusters mid-provisioning are undisturbed.

## Allow-listing your PVE host

The UI extension talks to the PVE API through Rancher's `/meta/proxy`, and that
proxy only forwards to hosts in the driver's `whitelistDomains`. Until your PVE
host is listed, **Test Connection** on the cloud credential fails with *"Rancher
could not reach the Proxmox VE server"* and the template/storage/bridge
dropdowns stay empty:

```bash
helm upgrade pve-rancher-driver deploy/chart -n cattle-system --reuse-values \
  --set 'nodeDriver.whitelistDomains={github.com,objects.githubusercontent.com,release-assets.githubusercontent.com,pve.example.com}'
```

Pass the **whole list**. Helm's `--set nodeDriver.whitelistDomains[3]=…`
replaces the list instead of appending to it, leaving the three GitHub entries
rendered as empty strings — which breaks the binary download.

Use the **hostname only** — no scheme and no `:8006`; Rancher matches the URL's
hostname, so `pve.example.com:8006` never matches. Wildcards (`*.example.com`)
work. Note the driver binary itself does not go through this proxy, so a driver
that downloaded fine can still fail here.

No reinstall is needed — the `NodeDriver` is a live resource and can also be
edited in place:

```bash
kubectl patch nodedriver.management.cattle.io pve --type=json \
  -p '[{"op":"add","path":"/spec/whitelistDomains/-","value":"pve.example.com"}]'
```

If you edit it in place, add the host to your Helm values too, or the next
`helm upgrade` will revert it.

## Air-gapped / self-hosted binaries

Host the binary somewhere your Rancher pods can reach, then:

```yaml
release:
  baseUrl: https://artifacts.internal/pve-rancher-driver
nodeDriver:
  whitelistDomains:
    - artifacts.internal
  checksum: "<sha256 of your hosted binary>"
  checksumFor: "v0.2.0" # must match the version being deployed
```

The URL is built as
`{{ release.baseUrl }}/<tag>/{{ release.binaryName }}-{{ nodeDriver.arch }}`,
so mirror that layout.

Full setup guide, including cloud credentials and machine pool fields:
[docs/rancher-setup.md](../../docs/rancher-setup.md).
