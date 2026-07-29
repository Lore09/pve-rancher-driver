# A controlled DHCP network for Kubernetes nodes

Nodes get their address from DHCP — the driver discovers it through the QEMU
guest agent and hands it to Rancher. "Controlled" here means that DHCP serves
the nodes from a scope that serves *only* nodes: a lease-pool exhaustion or a
bad reservation cannot then take out the rest of your LAN, and you can carve
out a predictable static range for the cluster VIP, a MetalLB pool and anything
else that must not move.

This guide assumes the setup this driver was developed against:

- a single PVE host with two physical NICs,
- `vmbr1` as the LAN bridge,
- **OPNsense running as a VM on that host**, with its LAN interface on `vmbr1`.

So OPNsense is already the router and DHCP server for everything on `vmbr1`.
The node network is added as a second interface on that same OPNsense VM, which
is what makes both paths below work without touching your existing LAN
addressing at all.

Two paths are documented. Path A is the default recommendation; take Path B
only if the node network has to exist beyond this one host.

---

## Path A — host-local port-less bridge (recommended)

A bridge with no physical port. VLAN tags never enter the picture, no switch is
involved, and — the reason to prefer it — **nothing about `vmbr0` or `vmbr1`
changes**, so there is no way to lose access to the host while setting it up.

### A.1 Create the bridge on the PVE host

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

### A.2 Give the OPNsense VM a leg in it

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

### A.3 Point machine pools at it

| Field | Value |
|---|---|
| Network Bridge (`pve-net-bridge`) | `vmbr2` |
| VLAN Tag (`pve-net-vlan-tag`) | `0` (untagged) |
| `pve-ipconfig` | `ip=dhcp` |

### A.4 The one limitation

The segment exists only on this host. A second PVE node cannot reach it, and
neither can bare metal. If that changes, migrate to Path B — see below; it does
not require re-provisioning the cluster.

---

## Path B — VLAN-aware `vmbr1` with an uplink trunk

Take this path when the node network must span several PVE hosts or reach
physical gear. It **modifies the bridge your LAN depends on**, so read the
warnings before applying.

### B.1 Make `vmbr1` VLAN-aware

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

### B.2 Trunk the VLAN on the switch

On the switch port facing `eno2`, set the port to trunk mode and allow VLAN 20
(tagged). The native/untagged VLAN stays whatever your LAN already uses.

Skip this step if you only want the VLAN to exist on this host for now — the
tags simply never leave the bridge, and you can trunk it later without touching
anything else.

### B.3 Add the VLAN interface in OPNsense

1. **Interfaces → Other Types → VLAN** — parent `vtnet1` (the LAN NIC), tag
   `20`, description `K8S`.
2. **Interfaces → Assignments** — assign the new VLAN interface as `K8S`.
3. From here, steps 2–5 of [A.2](#a2-give-the-opnsense-vm-a-leg-in-it) are
   identical: static address, DHCP scope with a reserved low range, and
   firewall rules in both directions.

### B.4 Point machine pools at it

| Field | Value |
|---|---|
| Network Bridge (`pve-net-bridge`) | `vmbr1` |
| VLAN Tag (`pve-net-vlan-tag`) | `20` |
| `pve-ipconfig` | `ip=dhcp` |

---

## Migrating from Path A to Path B

Both segments can exist at the same time, so this is a per-pool cutover with no
cluster rebuild:

1. Do B.1–B.3 with a **different subnet** from `vmbr2` (say `10.10.30.0/24`),
   so the two never overlap.
2. Edit one machine pool's Network Bridge and VLAN Tag, then scale it down and
   back up. New nodes land on the VLAN; existing nodes keep their `vmbr2`
   addresses and keep working, because OPNsense routes between the two.
3. Repeat pool by pool, control plane last.
4. When no pool references `vmbr2`, remove its NIC from the OPNsense VM and
   delete the bridge stanza.

---

## MTU

`pve-net-mtu` is only applied when `pve-net-bridge` is set — the driver only
rewrites the NIC when a bridge is named, and silently ignoring an MTU would
look like it had been honoured.

On Path B the MTU must not exceed that of the bridge's physical uplink, and the
switch has to agree. On Path A the segment never touches a physical NIC, so you
can raise it independently of your LAN.

Either way, keep it **identical across every node in a cluster**. A mismatch
does not fail cleanly: small packets get through, so the cluster comes up and
looks healthy, and then pods hang on large transfers.
