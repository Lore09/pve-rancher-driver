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
| `pve-pool` | *(empty)* | PVE resource pool new VMs are created into. See [rancher-setup.md](rancher-setup.md#restricting-the-token-to-a-resource-pool) |
| `pve-skip-permission-check` | `false` | Skip the token-privilege probe in `PreCreateCheck` |

The first five are **cloud credential** fields in Rancher, not machine-pool
fields: they are collected once on the credential and reused by every machine
pool that references it. `pve-pool` is among them because which pool a VM may
be created in is a property of the token's ACL — a token scoped to
`/pool/<name>` can only ever create VMs there.

## Placement and identity

| Flag | Default | Description |
|------|---------|-------------|
| `pve-node` | *(first online)* | Target PVE node name |
| `pve-template-vmid` | *(required)* | Template VMID to clone from. Mutually exclusive with `pve-template-tag` |
| `pve-template-tag` | *(empty)* | Select the template by PVE tag instead of VMID, e.g. `rancher-node`. Comma-separated tags must all match, and exactly one template must match. See below |
| `pve-template-tag-match` | `subset` | `subset` (template carries at least the given tags) or `exact` (its tags are exactly the given ones) |
| `pve-linked-clone` | `false` | Clone as a linked clone instead of a full clone. See below |
| `pve-clone-storage` | *(template's own)* | PVE storage id the clone's disks are created on, e.g. `ceph-rbd`. Full clones only. See below |
| `pve-clone-format` | *(storage default)* | Disk format for the clone: `raw`, `qcow2` or `vmdk`. Full clones only. See below |
| `pve-vmid` | `0` | Explicit VMID for the created VM, `0` = auto-assigned. Only meaningful for a single machine; mutually exclusive with `pve-vmid-range` |
| `pve-vmid-range` | *(empty)* | Allocate the VMID from this inclusive range, e.g. `200-299`. Empty lets Proxmox pick the next free id cluster-wide. See below |
| `pve-allowed-nodes` | *(empty)* | Comma-separated PVE node names the driver may place new VMs on, e.g. `pve1,pve2`. Empty considers every online node. Mutually exclusive with `pve-node`. See below |
| `pve-tags` | *(empty)* | Comma-separated PVE tags applied to the VM, e.g. `rancher,prod`. Informational only — for finding/filtering VMs in the PVE UI |
| `pve-description` | *(default text)* | VM Notes field. Empty writes a line naming the machine, the template it came from and the driver. See below |
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

### Where the clone's disks land

By default PVE creates a clone's disks on **the storage the template's disks
are on**. That is an inherited default, not a chosen one, and it pins every
machine pool cloning a given template to wherever that template happens to
live: putting nodes on Ceph when the template sits on `local-lvm` would
otherwise mean building a second template.

`pve-clone-storage` names the destination storage instead — the same thing as
the *Target Storage* dropdown in PVE's own clone dialog. Three cases where it
matters:

- **Different backends per pool.** One template, worker nodes on fast local
  NVMe, control-plane nodes on shared storage.
- **Linked clones on a template that cannot support them.** `pve-linked-clone`
  needs snapshot-capable storage; if the template lives on plain LVM, full-clone
  it once onto LVM-thin/ZFS with this flag and everything downstream works.
- **Capacity.** Ten full clones of a 20 GB template is 200 GB, and without this
  it all lands wherever the template is.

`pve-clone-format` (`raw`, `qcow2`, `vmdk`) picks the disk format. Leave it
empty unless you have a specific reason: the format is only selectable on
file-based storages (dir, NFS, CIFS), and block backends (LVM, ZFS, Ceph RBD)
reject anything but their own. The one common use is forcing `qcow2` on NFS so
the VMs support snapshots.

Two constraints:

- **Both are full-clone-only.** A linked clone is an overlay on the template's
  own disk, so it has no separate storage to land on and no format to choose.
  Combining either with `pve-linked-clone` is rejected in `PreCreateCheck`.
- **Neither lifts the cross-node rule.** Setting a destination storage does not
  let a template on local storage be cloned to a different node — PVE still
  requires the source template on shared storage for that, exactly as described
  under [`pve-allowed-nodes`](#pve-allowed-nodes).

### Selecting the template by tag

`pve-template-vmid` pins a machine pool to one specific template VM. That is
fine until you rebuild the image: the new template gets a new VMID, and every
machine pool that should use it has to be edited — and a pool still pointing at
a deleted VMID fails to provision with a Proxmox-side error.

`pve-template-tag` names the *image* instead. Tag the template `rancher-node`
in the PVE UI, set `pve-template-tag=rancher-node`, and rolling out a rebuilt
image is one operation on the PVE side: remove the tag from the old template,
put it on the new one. Machines created after that clone the new one; nothing
in Rancher changes.

Resolution happens per machine, at create time — not once when the pool is
saved. Retagging halfway through a scale-up means the machines already created
keep the old image and the ones after it get the new one, which is what you
want for a rolling image update and worth knowing before scaling a pool during
a rebuild.

**Exactly one template must match**, or provisioning fails naming every
candidate. This is deliberate: two templates carrying the same tag is what a
half-finished rollout looks like, and quietly picking one would build half a
machine pool from each image. Matching is:

| Policy | Matches when |
|---|---|
| `subset` (default) | The template carries **at least** the tags you listed. Extra tags — `debian13`, a build date — are ignored |
| `exact` | The template's tag set is **exactly** the one you listed |

Use `subset` unless you deliberately tag templates as complete sets. Note that a
token which cannot see a template's node gets an empty list rather than an
error, so "no template matches" can also mean a missing ACL.

### `pve-description`

Cloning copies the template's Notes field, so without this every machine in PVE
carries text describing the *template* — build date, image recipe, "do not
start this VM". The driver overwrites it with a line naming the machine, the
template VMID it was cloned from, and the fact that Rancher manages it, so an
unfamiliar VM in the PVE UI can be identified without cross-referencing
Rancher. Set `pve-description` to replace that text with your own.

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
| `pve-cloudinit-timeout` | `300` | Seconds to wait for cloud-init to finish inside the guest before handing the machine to Rancher. `0` skips the wait. See below |
| `pve-provision-delay` | `30` | Seconds to wait after the VM is up before handing it to Rancher for provisioning. See below |

The four settings marked *Requires `pve-net-bridge`* are only written while
rewriting the NIC, which only happens when a bridge is named. `PreCreateCheck`
rejects them without one rather than silently ignoring a VLAN you asked for.

See [networking.md](networking.md) for how to build the node network itself.

### `pve-cloudinit-timeout`

After the VM is up and its address is known, the driver SSHes in and runs
`cloud-init status --wait`, blocking until the guest itself reports that
cloud-init has finished. This is the readiness signal `pve-provision-delay`
only approximates: instead of guessing at a duration, the driver waits for the
thing it actually cares about — the resolver written, the default route
installed, the login user created.

It runs **before** data-disk setup, so the driver never partitions and mounts
while cloud-init is still growing the root filesystem or rewriting fstab.

Outcomes:

| In the guest | Driver behaviour |
|---|---|
| cloud-init finished cleanly | Continues |
| Finished with recoverable errors (a failed optional module) | Continues, logging a warning — worth checking first if the node later misbehaves |
| cloud-init is not installed | Skips the wait, logging that it did |
| cloud-init failed | Create fails, rather than handing Rancher a broken guest |
| Did not finish within the timeout | Create fails, naming this flag |

Set it to `0` to skip the wait entirely. With the wait on, `pve-provision-delay`
is usually redundant and can be lowered or set to `0` — the delay exists
precisely because the driver had no way to know when cloud-init was done.

The wait needs SSH, which means the cloud-init key injection has to have worked;
a template whose `pve-ssh-user` is wrong fails here instead of failing later
during Rancher's bootstrap, which is a considerably clearer error.

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
