# Cluster networking

Two decisions here are **independent**, and the guide is split along them:

1. **Which segment the nodes live on** — how the layer-2 network exists at all.
2. **How the nodes get addresses** — DHCP, DHCP with reservations, or static
   addresses derived by the driver.

Any segment works with any addressing mode. Pick one from Part 1 and one from
Part 2; nothing in either part constrains the other.

## The assumed setup

This guide assumes the setup this driver was developed against:

- a single PVE host with two physical NICs,
- `vmbr1` as the LAN bridge,
- **OPNsense running as a VM on that host**, with its LAN interface on `vmbr1`.

So OPNsense is already the router and DHCP server for everything on `vmbr1`.
Where a segment below needs its own router, it is added as a second interface on
that same OPNsense VM, which is what makes it work without touching your
existing LAN addressing at all.

---

# Part 1 — Choosing a segment

## Option 1: Flat, on the existing LAN

The simplest thing that works, and where most people start. The nodes join the
LAN broadcast domain you already have, get addresses from the DHCP scope you
already run, and **no host network changes are needed at all**.

The trade-off is that there is no isolation. The nodes share a lease pool with
your phones, laptops and printers, so a lease-pool exhaustion or a bad
reservation affects everything, and a broadcast storm from a node is your whole
LAN's problem. Node traffic is also not separable at the firewall, because there
is no separate interface to write rules against.

Machine pool: **leave Network Bridge (`pve-net-bridge`) empty** so the clone
inherits the template's NIC and the bridge it is already attached to. With no
bridge named the driver does not rewrite the net device at all, which also means
`pve-net-vlan-tag`, `pve-net-mtu`, `pve-net-model` and `pve-net-firewall` cannot
be used — they are only written while rewriting the NIC.

## Option 2: Host-local port-less bridge (recommended)

A bridge with no physical port. VLAN tags never enter the picture, no switch is
involved, and — the reason to prefer it — **nothing about `vmbr0` or `vmbr1`
changes**, so there is no way to lose access to the host while setting it up.

### 2.1 Create the bridge on the PVE host

Append to `/etc/network/interfaces`. Do not edit the existing `vmbr0`/`vmbr1`
stanzas:

```
auto vmbr2
iface vmbr2 inet manual
    bridge-ports none
    bridge-stp off
    bridge-fd 0
#k8s node network (host-local, routed by OPNsense)
```

Apply and verify:

```bash
ifreload -a          # brings up the new bridge; vmbr0/vmbr1 are untouched
ip link show vmbr2
```

`ifreload -a` is the Proxmox (ifupdown2) way to apply interface changes.
`ifquery --check vmbr2` reports whether the running state matches the file, and
`ifup --no-act vmbr2` dry-runs the stanza if you want to check syntax before
applying.

The bridge is deliberately `inet manual`: the PVE host itself needs no address
on the node network.

### 2.2 Give the OPNsense VM a leg in it

```bash
qm set <opnsense-vmid> --net2 virtio,bridge=vmbr2
```

Hot-plug works on a running OPNsense, but the interface will not be recognized
until you assign it. Inside OPNsense:

1. **Interfaces → Assignments** — the new NIC appears as `vtnet2`. Add it and
   name it `K8S`.
2. **Interfaces → K8S** — tick *Enable*, set IPv4 to *Static*, address
   `10.10.20.1/24`. Save and apply.
3. **Services → DHCPv4 → K8S** — enable, range `10.10.20.50` to
   `10.10.20.250`. That deliberately leaves `10.10.20.2`–`10.10.20.49` free for
   the cluster VIP (kube-vip), a MetalLB pool, and anything else static.
4. **Firewall → Rules → K8S** — allow `K8S net` to `any`. Nodes need to reach
   the internet for images and to reach Rancher.
5. **Firewall → Rules → LAN** — allow `LAN net` to `K8S net`, so you and the
   Rancher server can reach the nodes.

No NAT rule and no static route on any client is needed: LAN clients already
use OPNsense as their default gateway, and OPNsense has an interface in both
networks, so it routes between them directly.

### 2.3 The one limitation

The segment exists only on this host. A second PVE node cannot reach it, and
neither can bare metal. If that changes, migrate to Option 3 — see
[Part 5](#part-5--migrating-between-segments); it does not require
re-provisioning the cluster.

## Option 3: VLAN-aware bridge with an uplink trunk

Take this option when the node network must span several PVE hosts or reach
physical gear. It **modifies the bridge your LAN depends on**, so read the
warnings before applying.

### 3.1 Make `vmbr1` VLAN-aware

```
iface vmbr1 inet manual
    bridge-ports eno2
    bridge-stp off
    bridge-fd 0
    bridge-vlan-aware yes
    bridge-vids 2-4094
```

> **Three ways this bites you, in order of likelihood:**
>
> - `ifreload -a` briefly interrupts the bridge. Run it from the PVE web
>   console or IPMI — **never** over an SSH session that rides `vmbr1`.
> - Check where the PVE management address lives first (`ip -br addr`). If it
>   is on the bridge you are about to edit, you must have out-of-band access.
> - Untagged VMs are unaffected: a VLAN-aware Linux bridge gives untagged
>   traffic the default PVID of 1, so existing guests keep working exactly as
>   before. This is worth verifying rather than trusting — bring one up and
>   ping it after applying.

### 3.2 Trunk the VLAN on the switch

On the switch port facing `eno2`, set the port to trunk mode and allow VLAN 20
(tagged). The native/untagged VLAN stays whatever your LAN already uses.

Skip this step if you only want the VLAN to exist on this host for now — the
tags simply never leave the bridge, and you can trunk it later without touching
anything else.

### 3.3 Add the VLAN interface in OPNsense

1. **Interfaces → Other Types → VLAN** — parent `vtnet1` (the LAN NIC), tag
   `20`, description `K8S`.
2. **Interfaces → Assignments** — assign the new VLAN interface as `K8S`.
3. From here, steps 2–5 of
   [2.2](#22-give-the-opnsense-vm-a-leg-in-it) are identical: static address,
   DHCP scope with a reserved low range, and firewall rules in both directions.

## Segment comparison

| | Isolation | Spans hosts | Reaches physical gear | Risk of locking yourself out | Host changes needed |
|---|---|---|---|---|---|
| **1. Flat LAN** | None — shares your LAN | Yes (it is your LAN) | Yes | None | None |
| **2. Host-local bridge** | Full — own subnet and firewall interface | No | No | None — `vmbr0`/`vmbr1` untouched | One new bridge stanza |
| **3. VLAN-aware bridge** | Full — own subnet and firewall interface | Yes | Yes | **Real** — edits the bridge your LAN rides | Edit `vmbr1`, plus a switch trunk port |

### Which bridge to name on the machine pool

| Segment | Network Bridge (`pve-net-bridge`) | VLAN Tag (`pve-net-vlan-tag`) |
|---|---|---|
| Flat LAN | *(empty — inherit the template's NIC)* | *(not usable without a bridge)* |
| Host-local bridge | `vmbr2` | `0` (untagged) |
| VLAN-aware bridge | `vmbr1` | `20` |

---

# Part 2 — Choosing addressing

All three options work on any segment from Part 1.

## Option 1: DHCP from a dedicated scope (recommended default)

The driver's default. The VM boots, DHCP hands it an address, and the driver
discovers that address through the QEMU guest agent and reports it to Rancher.
Nothing has to be reserved or planned per node.

"Dedicated scope" means the nodes are served by a scope that serves *only*
nodes — which follows automatically from segment option 2 or 3. Reserve a low
static block inside it (`10.10.20.2`–`10.10.20.49` in the examples above) and
keep it outside the lease range, so the cluster VIP, a MetalLB pool and anything
else that must not move has somewhere to live.

Machine pool: **Addressing (`pve-ip-mode`) = `dhcp`** (or leave it unset — a
pool created before the flag existed keeps this behaviour).

## Option 2: DHCP with per-host reservations

Reservations give predictable addresses, but **this does not automate**, and the
reason is a chicken-and-egg problem worth stating plainly:

- A DHCP reservation is keyed on the VM's MAC address.
- The MAC does not exist until the VM does — it is generated by PVE when the
  clone is created.
- Setting **Network Bridge** (`pve-net-bridge`) makes PVE rewrite the net
  device, and it **regenerates the MAC** in the process. So even a MAC copied
  from the template is not the MAC the node ends up with.

The consequence: a reservation can only be created **after** a node exists, and
the node is already holding a different lease at that point, so the reservation
takes effect only after a lease renewal or a reboot. There is no point in the
provisioning flow where you can get ahead of it.

That is fine for a handful of long-lived, hand-pinned nodes. It is unworkable
for a pool that scales, where every new machine needs a manual reservation and a
reboot before its address is what you planned. If you want predictable addresses
on a scaling pool, use option 3.

## Option 3: Static, driver-assigned

The driver computes each machine's address instead of asking for one. It is a
separate process per machine with no shared state, so there is nothing to
allocate against — the address is **derived** from the VMID:

```
address = pve-ip-base + (vmid - <low end of pve-vmid-range>)
```

Worked example, with `pve-vmid-range 200-299`, `pve-ip-base 10.10.20.10/24` and
`pve-gateway 10.10.20.1`:

| VMID | Address |
|---|---|
| 200 | `10.10.20.10/24` |
| 201 | `10.10.20.11/24` |
| 202 | `10.10.20.12/24` |
| 299 | `10.10.20.109/24` |

Deleting a machine frees its VMID and therefore its address; the next machine
reclaims both.

Requirements and rules:

- **`pve-vmid-range` is required.** Without it the VMID comes from
  `/cluster/nextid`, is unbounded, and the offset is meaningless.
- **Cloud-init is always on.** The address is delivered through cloud-init
  `ipconfig0`, and cloud-init writes it persistently, so it survives reboots.
  The driver enables cloud-init unconditionally, so there is nothing to set.
- **`pve-gateway` must be inside the subnet of `pve-ip-base`.** A gateway the
  nodes cannot reach is almost always a typo, so it is rejected.
- **Give each pool its own `pve-ip-base`.** VMIDs are unique cluster-wide (the
  driver scans `/cluster/resources`), so two pools sharing a VMID range get
  different VMIDs and different addresses — that is safe. The collision is two
  pools with *different* range minima but the *same* base: `200-299` and
  `300-399` both based at `10.10.20.10/24` give VMID 200 and VMID 300 offset 0,
  so both claim `10.10.20.10`. Use a distinct base per pool, or share both the
  range and the base.
- **Exclude the static block from the DHCP scope.** If the scope in Part 1
  leases `10.10.20.50`–`10.10.20.250`, a static base of `10.10.20.10/24` with a
  100-wide VMID range stays clear of it. Overlap means DHCP eventually hands a
  static node's address to something else.

### The subnet caps the pool, not the VMID range

VMIDs are handed out lowest-free-first, so machines fill the subnet upward from
the base and the subnet only ever has to hold the machines running at once. A
VMID range **wider** than the subnet is therefore normal and accepted — it just
supplies ids.

So size the subnet by how many nodes you intend to run, and the VMID range by
how much id headroom you want. With `pve-ip-base 10.10.20.9/30` (a `/30` spans
`.8`–`.11`, leaving `.9` and `.10` usable) and `pve-vmid-range 200-299`, the pool
holds two machines. The third fails with `static IP pool exhausted: ... leaves
room for 2 machines`, naming the capacity so it is actionable.

`PreCreateCheck` rejects only a base that is unusable outright — the network or
broadcast address of its subnet, or one with no room before the subnet ends —
and logs the real capacity when the VMID range exceeds it.

Machine pool: **Addressing (`pve-ip-mode`) = `static`**, **Base address
(`pve-ip-base`)**, **Gateway (`pve-gateway`)**, plus a **VMID range
(`pve-vmid-range`)**.

## Addressing comparison

| | Predictable addresses | Works while scaling | External dependency | Setup effort |
|---|---|---|---|---|
| **1. DHCP from a scope** | No | Yes | A DHCP server | None beyond the scope |
| **2. DHCP + reservations** | Yes, eventually | **No** — manual per node, after the fact | A DHCP server | Per-node manual work forever |
| **3. Static, driver-assigned** | Yes, from first boot | Yes | None | A VMID range, a base and a gateway, once |

---

# Part 3 — DNS

`pve-nameservers` and `pve-searchdomain` apply in **both** addressing modes:

| Field | Example | Notes |
|---|---|---|
| `pve-nameservers` | `10.10.20.1 1.1.1.1` | Space- or comma-separated. Empty keeps whatever the DHCP lease supplies |
| `pve-searchdomain` | `cluster.lan` | |

Both require **`pve-cloudinit`**, because PVE applies them as cloud-init
`nameserver`/`searchdomain` options; with cloud-init off they would be silently
dropped, so the driver rejects them instead. Each nameserver entry is parsed as
an IP address — PVE stores the string without validating it, so a hostname or a
typo would be accepted by the API and only surface later as a node that cannot
resolve anything.

This is the useful combination worth knowing about: keep **DHCP** addressing and
still point the nodes at an internal resolver, without moving to static
addresses to get there.

---

# Part 4 — MTU

`pve-net-mtu` is only applied when `pve-net-bridge` is set — the driver only
rewrites the NIC when a bridge is named, and silently ignoring an MTU would
look like it had been honoured. On the flat-LAN segment, where no bridge is
named, it therefore cannot be used at all.

On the VLAN segment the MTU must not exceed that of the bridge's physical
uplink, and the switch has to agree. On the host-local bridge the segment never
touches a physical NIC, so you can raise it independently of your LAN.

Either way, keep it **identical across every node in a cluster**. A mismatch
does not fail cleanly: small packets get through, so the cluster comes up and
looks healthy, and then pods hang on large transfers.

---

# Part 5 — Migrating between segments

Both segments can exist at the same time, so moving from the host-local bridge
(option 2) to the VLAN (option 3) is a per-pool cutover with no cluster rebuild:

1. Do 3.1–3.3 with a **different subnet** from `vmbr2` (say `10.10.30.0/24`),
   so the two never overlap.
2. Edit one machine pool's Network Bridge and VLAN Tag, then scale it down and
   back up. New nodes land on the VLAN; existing nodes keep their `vmbr2`
   addresses and keep working, because OPNsense routes between the two.
3. Repeat pool by pool, control plane last.
4. When no pool references `vmbr2`, remove its NIC from the OPNsense VM and
   delete the bridge stanza.

Addressing is an independent choice and **does not have to change with the
segment**. A pool can move from `vmbr2` to the VLAN and stay on DHCP throughout.
If it is on static addressing, the one thing that must change is `pve-ip-base`
and `pve-gateway`, because the new segment is a different subnet — the VMID
range, and therefore the ordering of the addresses, stays exactly as it was.
