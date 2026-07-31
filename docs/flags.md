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
| `pve-linked-clone` | `false` | Clone as a linked clone instead of a full clone. See below |
| `pve-vmid` | `0` | Explicit VMID for the created VM, `0` = auto-assigned. Only meaningful for a single machine; mutually exclusive with `pve-vmid-range` |
| `pve-vmid-range` | *(empty)* | Allocate the VMID from this inclusive range, e.g. `200-299`. Empty lets Proxmox pick the next free id cluster-wide. See below |
| `pve-allowed-nodes` | *(empty)* | Comma-separated PVE node names the driver may place new VMs on, e.g. `pve1,pve2`. Empty considers every online node. Mutually exclusive with `pve-node`. See below |
| `pve-pool` | *(empty)* | PVE resource pool new VMs are created into. See [rancher-setup.md](rancher-setup.md#restricting-the-token-to-a-resource-pool) |
| `pve-tags` | *(empty)* | Comma-separated PVE tags applied to the VM, e.g. `rancher,prod`. Informational only — for finding/filtering VMs in the PVE UI |
| `pve-vm-name-prefix` | *(empty)* | Prefix for the PVE VM name, rendered as `<prefix>-<machine name>`. Empty uses the machine name unchanged. Letters, digits and inner hyphens only — PVE validates the result as a DNS name, and the whole name must fit 63 characters |
| `pve-onboot` | `false` | Start the VM automatically when the PVE host boots |

### How VMIDs are allocated

With neither flag set, the driver asks Proxmox for the next free id
(`/cluster/nextid`), which is the lowest unused id from 100 upwards.

With `pve-vmid-range`, the driver picks the lowest free id inside the range
itself, checking every VM **and container** in the cluster — Proxmox shares one
id space between them, and templates occupy ids too.

That check is advisory: an id is free when it is chosen and can be taken before
the clone lands, which a pool creating several machines at once will
occasionally hit. The driver retries up to 5 times with a freshly chosen id, so
the race is invisible in normal use. If the range fills up, provisioning fails
with a clear error rather than silently spilling outside it.

Valid ids are `100`-`999999999`; Proxmox reserves `1`-`99`.

### `pve-linked-clone`

By default the driver does a **full clone**: a complete, independent copy of
the template's disk. That copy is real storage I/O proportional to the
template's size, and when several machines in a pool provision at once, their
clones contend for the same storage — under load this can push total
provisioning time past Rancher's node-startup timeout, which then deletes and
recreates the not-yet-ready machine, repeating indefinitely.

`pve-linked-clone` clones as a **linked clone** instead: a thin overlay that
only stores blocks that differ from the template, created almost instantly
regardless of template size.

**Before enabling it, understand the tradeoff:**

- The template can never be deleted, and its disk can never be modified,
  while any linked clone made from it still exists — Proxmox enforces this.
- The template's storage becomes a dependency for every VM cloned from it: if
  that storage degrades or saturates, every linked clone degrades with it.
- Not every storage backend supports linked clones — it needs snapshot
  capability (LVM-thin, ZFS, Ceph RBD, qcow2 on a snapshot-capable
  filesystem). Plain LVM or raw NFS typically cannot do it, and the clone
  will fail outright.

### `pve-allowed-nodes`

With `pve-node` empty, the driver picks a destination node itself: the
online node with the most free memory, restricted to `pve-allowed-nodes`
when that is set (otherwise every online node is a candidate). On a
single-node install there is only ever one candidate, so this has no
observable effect — it only matters once there is more than one node to
choose between.

The node a VM actually lands on is written back into the machine's stored
`pve-node` value once `Create` succeeds, so every later operation
(`Start`/`Stop`/`GetState`/`Remove`, each a separate driver invocation) talks
to the right node instead of re-running selection and potentially picking a
different one.

The template being cloned does not need to live on the chosen destination
node — its actual node is discovered independently at clone time. Proxmox
only allows a clone whose destination differs from the template's own node
when the template's disk is on **shared storage** (e.g. Ceph, NFS); on local
storage that clone fails with a Proxmox-side error naming the mismatch.

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
| `pve-provision-delay` | `30` | Seconds to wait after the VM is up before handing it to Rancher for provisioning. See below |

The four settings marked *Requires `pve-net-bridge`* are only written while
rewriting the NIC, which only happens when a bridge is named. `PreCreateCheck`
rejects them without one rather than silently ignoring a VLAN you asked for.

See [networking.md](networking.md) for how to build the node network itself.

### `pve-provision-delay`

`Create` sleeps for this long, after the VM is running and reachable, before
returning. Returning is what hands the machine to Rancher, and everything
after that point — waiting for SSH, detecting the OS, running the bootstrap
commands — is Rancher's own provisioning code that the driver cannot hook
into. Delaying here is the only lever the driver has over when that starts.

It defaults to 30 seconds rather than 0 because **"sshd accepts a
connection" is not the same as "the guest is ready"**. Several cloud images
open port 22 before cloud-init has finished writing the resolver and the
default route. Rancher fires its first bootstrap command the moment SSH
answers, so with no delay that command can run against a guest with no
working DNS and fail on something that would have succeeded seconds later —
and a failed bootstrap deletes and recreates the entire machine, often in a
loop.

Raise it if bootstrap still fails on a slow-booting image; set it to `0` to
hand the machine back immediately.

## Cloud-init and access

| Flag | Default | Description |
|------|---------|-------------|
| `pve-cloudinit` | `true` | Deprecated and always on. Cloud-init is the only channel for the SSH key, the static address and the DNS settings, so the driver enables it regardless |
| `pve-ip-mode` | `dhcp` | `dhcp` or `static`. Static derives each machine's address from its VMID, so it requires `pve-vmid-range` |
| `pve-ip-start` | — | First address of the static pool, e.g. `192.168.15.150`. The lowest VMID in the range gets this address; each later VMID gets the next |
| `pve-ip-end` | — | Last address of the pool, e.g. `192.168.15.159`. The pool size caps how many machines the machine pool can hold |
| `pve-ip-prefix` | — | Subnet prefix length the machines get, e.g. `24`. **The netmask, not the pool size** — see below |
| `pve-gateway` | — | Default gateway. May sit outside the pool, but must be inside the subnet `pve-ip-prefix` describes |
| `pve-nameservers` | — | DNS servers, space- or comma-separated. Applies in both modes; empty keeps the DHCP-supplied resolver |
| `pve-searchdomain` | — | DNS search domain, e.g. `cluster.lan`. Applies in both modes |
| `pve-ciuser` | *(empty)* | Cloud-init user to create and install the keys for. Empty means "same as `pve-ssh-user`", which is nearly always what you want |
| `pve-sshkeys` | *(empty)* | Extra OpenSSH public keys, one per line. The machine's own generated key is always injected as well |
| `pve-ssh-user` | `root` | Account the driver and Rancher log in as. **Must be a user that exists in the guest** — `debian` for Debian's cloud image, `rancher` for Leap Micro. The `root` default works with neither |
| `pve-ssh-port` | `22` | SSH port |

`pve-ciuser` and `pve-ssh-user` are the same account: cloud-init installs the SSH
keys for `ciuser`, and that is the only account anything can subsequently log
into. Leaving `pve-ciuser` empty derives it from `pve-ssh-user`, which is why the
UI exposes a single **VM User** field.

### The prefix is the netmask, not the pool size

This is the one thing worth reading twice. The pool is bounded by
`pve-ip-start` and `pve-ip-end`; `pve-ip-prefix` is the netmask the machines
get — what they use to decide whether an address is reachable directly or has to
go via the gateway.

They are separate fields because folding them into a single CIDR invites writing
`/28` to mean "16 addresses". That does not bound the pool: it narrows the
subnet to `192.168.15.144–.159`, so a gateway at `192.168.15.1` is no longer
on-link, the default route cannot be installed, and the node boots unable to
reach anything.

So set `pve-ip-prefix` to the prefix of the **real network** — usually the same
`/24` your LAN uses — and bound the pool with start and end:

```
pve-ip-start  = 192.168.15.150
pve-ip-end    = 192.168.15.159
pve-ip-prefix = 24
pve-gateway   = 192.168.15.1
```

The gateway sits outside `.150–.159`, which is expected and fine. What is
rejected is a gateway outside the `/24`.

### How static addresses are allocated

The driver is a separate process per machine with no shared state, so there is
nothing to allocate against. The address is derived instead:

```
address = pve-ip-start + (vmid - <low end of pve-vmid-range>)
```

With `pve-vmid-range 200-299` and the pool above, VMID 200 gets
`192.168.15.150`, VMID 201 gets `.151`, and so on. Deleting a machine frees its
VMID and therefore its address.

**Give each pool its own address range.** VMIDs are unique across the whole
cluster — the driver picks one by scanning `/cluster/resources` — so two machine
pools drawing from the *same* VMID range always get different VMIDs and
therefore different addresses. That case is safe. The collision is the opposite
one: two pools with **different** range minima but the **same** `pve-ip-start`.
With `200-299` and `300-399` both starting at `192.168.15.150`, VMID 200 and
VMID 300 both compute offset 0 and both claim `.150`.

**The pool caps the machine count, not the VMID range.** VMIDs are handed out
lowest-free-first, so machines fill the pool from the start upward and it only
has to hold the machines running at once. A VMID range *wider* than the pool is
therefore normal — it just supplies ids. With the ten-address pool above and
`pve-vmid-range 200-299`, the eleventh machine fails with:

```
pve: static IP pool exhausted: 192.168.15.150-192.168.15.159 holds 10 machines,
and VMID 210 needs slot 11; widen the pool or scale down
```

`PreCreateCheck` rejects a pool whose ends fall in different subnets, one that
includes the subnet's network or broadcast address, an end below the start, or a
gateway outside the subnet. It logs the real capacity when the VMID range
exceeds it.

DNS (`pve-nameservers`, `pve-searchdomain`) requires `pve-cloudinit` in either
mode, because PVE applies `nameserver`/`searchdomain` as cloud-init options and
would otherwise drop them silently.

## Debugging

| Flag | Default | Description |
|------|---------|-------------|
| `pve-keep-on-failure` | `false` | Leave the cloned VM in place when Create fails, instead of rolling it back. Standalone CLI debugging only — in Rancher it leaks VMs |
