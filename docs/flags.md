# Flag reference

Every flag the driver declares. In Rancher these become machine-pool fields
automatically — the `pve-` prefix is stripped and the rest camel-cased, so
`pve-data-disk` is stored as `dataDisk` in the machine config. The polished form
from the UI extension renders the common ones; the rest are reachable by editing
the machine config YAML directly.

For which fields to set on a machine pool and why, see
[rancher-setup.md](rancher-setup.md#machine-pool-fields).

## Connection

| Flag | Default | Description |
|------|---------|-------------|
| `pve-api-url` | *(required)* | PVE REST API base URL, e.g. `https://host:8006/api2/json` |
| `pve-api-token-id` | *(required)* | PVE API token id (`USER@REALM!TOKENID`) |
| `pve-api-token-secret` | *(required)* | PVE API token secret |
| `pve-api-insecure` | `false` | Skip TLS certificate verification. Applies to the driver only — Rancher's UI proxy validates the certificate regardless |
| `pve-ca-cert` | *(empty)* | PEM CA certificate content (not a path) to trust for the PVE API |
| `pve-skip-permission-check` | `false` | Skip the token-privilege probe in `PreCreateCheck` |

## Placement and identity

| Flag | Default | Description |
|------|---------|-------------|
| `pve-node` | *(first online)* | Target PVE node name |
| `pve-template-vmid` | *(required)* | Template VMID to clone from |
| `pve-vmid` | `0` | Explicit VMID for the created VM, `0` = auto-assigned |
| `pve-vmname-prefix` | *(empty)* | Prefix for the PVE VM name, rendered as `<prefix>-<machine name>`. Empty uses the machine name unchanged. Letters, digits and inner hyphens only — PVE validates the result as a DNS name, and the whole name must fit 63 characters |
| `pve-onboot` | `false` | Start the VM automatically when the PVE host boots |

## Sizing

| Flag | Default | Description |
|------|---------|-------------|
| `pve-cores` | `2` | CPU cores per socket |
| `pve-sockets` | `1` | CPU sockets |
| `pve-memory` | `2048` | RAM in MB |

## Disks

| Flag | Default | Description |
|------|---------|-------------|
| `pve-boot-disk-size` | `0` | Grow the cloned boot disk to this size in GB (`0` = keep the template's size). PVE can only grow a disk, never shrink it |
| `pve-boot-disk-device` | `scsi0` | PVE config key of the boot disk grown by `pve-boot-disk-size` (`scsi0`, `virtio0`, `sata0`, ...) |
| `pve-data-disk` | *(empty)* | Data disk to attach; **repeatable**. Grammar below |
| `pve-disk-setup-timeout` | `300` | Seconds to wait for SSH plus formatting and mounting of the data disks |

### `pve-data-disk` grammar

Comma-separated `key=value` pairs, one flag occurrence per disk:

```
--pve-data-disk size=100,storage=local-lvm,fs=ext4,mount=/var/lib/longhorn
--pve-data-disk size=50,storage=ceph-rbd,fs=xfs,mount=/var/lib/rancher,backup=1
--pve-data-disk size=200,storage=local-lvm,fs=none
```

| Key | Default | Meaning |
|---|---|---|
| `size` | *required* | Size in GB, must be > 0 |
| `storage` | *required* | PVE storage id |
| `fs` | `ext4` | `ext4`, `xfs`, or `none` |
| `mount` | — | Absolute path. Required unless `fs=none`, and rejected when `fs=none` |
| `label` | `pvedata<N>` | Filesystem label, fstab key **and** PVE disk serial — this is how the guest finds the disk |
| `device` | first free slot | Explicit PVE config key, `scsi1`–`scsi30` |
| `discard` | `on` | Pass TRIM through to the storage |
| `iothread` | `1` | Dedicated I/O thread (needs `virtio-scsi-single`, which the template guide uses) |
| `backup` | `0` | Include in PVE backups. Off by default: a replicated Longhorn volume gains nothing from a host-level backup |

**`fs=none`** attaches the disk and leaves the guest completely alone — no
filesystem, no mount, no fstab entry. That is what Ceph/Rook OSDs and Longhorn's
v2 data engine want, since both require an unformatted block device and refuse
one that already has a filesystem. For anything that expects a directory,
including Longhorn v1, use `ext4` or `xfs`.

Mount paths are validated before anything is cloned. Rejected: the system
directories (`/`, `/etc`, `/usr`, `/var`, `/home`, `/root`, `/boot`, `/dev`,
`/proc`, `/sys`, `/bin`, `/sbin`, `/lib`, `/lib64`, `/run`) and everything
inside the kernel-managed ones; `..` segments; two disks on the same mount
point; and one disk nested inside another. `/var/lib/longhorn` is fine — only
the exact system paths are refused.

## Networking

| Flag | Default | Description |
|------|---------|-------------|
| `pve-net-device` | `net0` | PVE config device (`net0`..`net31`) whose MAC pins down IP discovery, and which the `pve-net-*` settings below are written to |
| `pve-net-bridge` | *(empty)* | PVE bridge to attach the NIC to, e.g. `vmbr1`. **Empty leaves the template's network untouched**; setting it rewrites `pve-net-device` |
| `pve-net-model` | `virtio` | Emulated NIC model. Requires `pve-net-bridge` |
| `pve-net-vlan-tag` | `0` | 802.1Q VLAN tag (`0` = untagged). Requires `pve-net-bridge` |
| `pve-net-mtu` | `0` | NIC MTU (`0` = PVE default). Requires `pve-net-bridge` |
| `pve-net-firewall` | *(empty)* | `true`/`false` to toggle the PVE firewall on the NIC; empty keeps the PVE default. Requires `pve-net-bridge` |
| `pve-net-iface` | *(empty)* | Restrict IP discovery to this guest interface name. Rarely needed — MAC matching already pins it |
| `pve-agent-timeout` | `300` | Seconds to wait for the QEMU guest agent to report an IP |

The four settings marked *Requires `pve-net-bridge`* are only written while
rewriting the NIC, which only happens when a bridge is named. `PreCreateCheck`
rejects them without one rather than silently ignoring a VLAN you asked for.

See [networking.md](networking.md) for how to build the node network itself.

## Cloud-init and access

| Flag | Default | Description |
|------|---------|-------------|
| `pve-cloudinit` | `false` | Push `ipconfig0` / `ciuser` / `sshkeys` to the cloned VM. **Required** for any data disk with a filesystem |
| `pve-ipconfig` | `ip=dhcp` | Cloud-init `ipconfig0` value. The UI forces DHCP; the driver discovers the resulting address through the guest agent |
| `pve-ciuser` | *(empty)* | Cloud-init user to create and install the keys for. Empty means "same as `ssh-user`", which is nearly always what you want |
| `pve-sshkeys` | *(empty)* | Extra OpenSSH public keys, one per line. The machine's own generated key is always injected as well |
| `ssh-user` | `root` | Account the driver and Rancher log in as. **Must be a user that exists in the guest** — `debian` for Debian's cloud image, `rancher` for Leap Micro. The `root` default works with neither |
| `ssh-port` | `22` | SSH port |

`pve-ciuser` and `ssh-user` are the same account: cloud-init installs the SSH
keys for `ciuser`, and that is the only account anything can subsequently log
into. Leaving `pve-ciuser` empty derives it from `ssh-user`, which is why the
UI exposes a single **VM User** field.

## Debugging

| Flag | Default | Description |
|------|---------|-------------|
| `pve-keep-on-failure` | `false` | Leave the cloned VM in place when Create fails, instead of rolling it back. Standalone CLI debugging only — in Rancher it leaks VMs |
