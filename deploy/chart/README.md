# Proxmox VE Node Driver

Registers the `pve` node driver with Rancher so RKE2/K3s machine pools can
provision Proxmox VE virtual machines cloned from a template.

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
| `nodeDriver.whitelistDomains` | 3 GitHub hosts | Replace entirely when self-hosting the binary |
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
helm uninstall <release> -n <namespace>
kubectl delete nodedriver.management.cattle.io pve
```

## Air-gapped / self-hosted binaries

Host the binary somewhere your Rancher pods can reach, then:

```yaml
release:
  baseUrl: https://artifacts.internal/pve-rancher-driver
nodeDriver:
  whitelistDomains:
    - artifacts.internal
  checksum: "<sha256 of your hosted binary>"
  checksumFor: "v0.1.3" # must match the version being deployed
```

The URL is built as
`{{ release.baseUrl }}/<tag>/{{ release.binaryName }}-{{ nodeDriver.arch }}`,
so mirror that layout.

Full setup guide, including cloud credentials and machine pool fields:
[docs/rancher-setup.md](../../docs/rancher-setup.md).
