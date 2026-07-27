# Preparing a Proxmox VE template for the `pve` node driver

Every node Rancher creates is a **full clone of one PVE template**, so the
template is where all guest-side requirements are baked in. This guide covers
two supported base images:

- **Debian 13 (trixie) `genericcloud`** — recommended default, classic mutable
  cloud image.
- **openSUSE Leap Micro 6.2 `Default-qcow`** — immutable/transactional OS for
  workloads that want a container-focused, self-healing base.

Both images were verified to contain everything the driver needs. Whichever
you pick, the four hard requirements are:

1. **`qemu-guest-agent` baked into the image itself.** The driver discovers
   the clone's IP exclusively through the guest agent; installing it via a
   first-boot script is too late — the driver starts polling the moment the
   clone boots.
2. **A cloud-init drive attached to the template** (`--ide2 <storage>:cloudinit`).
   The driver pushes network config, the cloud-init user and the SSH public
   key through it on every clone.
3. **A cloud-init user with passwordless `sudo`, plus `curl` and `bash` on
   `PATH`.** Rancher's system-agent bootstrap runs over SSH as this user
   *after* the driver has finished; if sudo prompts for a password or `curl`
   is missing, the VM boots and the driver reports success, but the node
   never reaches `Ready` in Rancher.
4. **DHCP reachable on the bridge** used for `net0` (or a static
   `--pve-ipconfig` you set yourself).

Everything below runs **on the PVE host** as root.

---

## Option A — Debian 13 (trixie)

### A.1 Download and customize the image

```bash
cd /var/lib/vz/template/iso    # any workdir is fine
wget https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2
apt-get install -y libguestfs-tools

# Bake the guest agent in. curl/bash/sudo already ship in genericcloud;
# install them anyway if you build from a smaller variant.
virt-customize -a debian-13-genericcloud-amd64.qcow2 \
  --install qemu-guest-agent \
  --run-command 'systemctl enable qemu-guest-agent'

# Optional: grow the base disk so nodes have room for container images.
qemu-img resize debian-13-genericcloud-amd64.qcow2 20G
```

> Use **`genericcloud`**, not `nocloud`: the `nocloud` variant omits
> cloud-init itself. The genericcloud image ships the `debian` user with
> passwordless sudo already configured.

### A.2 Create the template VM

```bash
export TMPL=9000
qm create $TMPL --name debian-13-tmpl --memory 2048 --cores 2 \
  --net0 virtio,bridge=vmbr0 \
  --scsihw virtio-scsi-single --agent 1 \
  --serial0 socket --vga serial0 --ostype l26

qm importdisk $TMPL debian-13-genericcloud-amd64.qcow2 local-lvm
qm set $TMPL --scsi0 local-lvm:vm-$TMPL-disk-0,discard=on,ssd=1 \
  --boot order=scsi0
qm set $TMPL --ide2 local-lvm:cloudinit
```

Notes:

- `--agent 1` tells PVE to expect the QEMU guest agent (matches requirement 1).
- `--ide2 local-lvm:cloudinit` attaches the cloud-init drive (requirement 2).
- `vmbr0` must be a bridge whose network hands out DHCP leases, unless you
  plan to pass static `--pve-ipconfig` per node.
- Do **not** set `--ciuser` for Debian: the image's default `debian` user is
  already correct. Set `--ssh-user debian` on the Rancher side.
- The template's DHCP/ssh config comes from the image; the driver adds
  `ipconfig0` and `sshkeys` per clone.

### A.3 Convert to template

```bash
qm template $TMPL
```

`$TMPL` (9000) is the value you put in **`pve-template-vmid`**.

---

## Option B — openSUSE Leap Micro 6.2 (immutable)

Leap Micro 6.2's `Default-qcow` appliance already contains
`cloud-init` (+ `cloud-init-config-suse`), `qemu-guest-agent`, `combustion`,
`ignition`, `curl`, `sudo` and `openssh-server` — verified against the
published SBOM (`...-Default-qcow-Build12.9.cdx.json`). That makes it usable
with the same driver flow as Debian; only the customization details differ.

### B.1 Download and customize the image

```bash
cd /var/lib/vz/template/iso
wget https://download.opensuse.org/distribution/leap-micro/6.2/appliances/openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2
apt-get install -y libguestfs-tools   # or: zypper install guestfs-tools

# Sanity-check the image really carries what the driver needs:
virt-ls -a openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2 /usr/sbin/ \
  | grep -E 'qemu-ga|cloud-init'

# qemu-guest-agent is installed but not necessarily enabled; enable it.
# /etc on Leap Micro is a writable overlay, so the symlink edit persists.
virt-customize -a openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2 \
  --run-command 'systemctl enable qemu-guest-agent'

qemu-img resize openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2 20G
```

### B.2 Create the template VM

```bash
export TMPL=9001
qm create $TMPL --name leapmicro-62-tmpl --memory 2048 --cores 2 \
  --net0 virtio,bridge=vmbr0 \
  --scsihw virtio-scsi-single --agent 1 \
  --serial0 socket --vga serial0 --ostype l26

qm importdisk $TMPL openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2 local-lvm
qm set $TMPL --scsi0 local-lvm:vm-$TMPL-disk-0,discard=on,ssd=1 \
  --boot order=scsi0
qm set $TMPL --ide2 local-lvm:cloudinit
qm set $TMPL --ciuser rancher
qm template $TMPL
```

The important difference from Debian: **Leap Micro has no default login
user**, so the template sets `--ciuser rancher`. PVE's cloud-init then creates
the `rancher` user (with passwordless sudo) on every clone, applies the
driver-injected SSH keys to it, and clones inherit the `ciuser` setting from
the template. Match it on the Rancher side with `--ssh-user rancher` (or set
`--pve-ciuser rancher` on the node template — either works; the driver flag
wins if both are set).

> Leap Micro is immutable: system packages are managed with
> `transactional-update`. Kubernetes itself runs as RKE2/K3s binaries +
> containers, so nothing extra needs installing for the Rancher use case —
> but if you need host-level tooling, bake it into the image the same way as
> above, never at runtime.

---

## Verify the template works before pointing Rancher at it

Clone once by hand and confirm the guest agent answers:

```bash
qm clone $TMPL 999 --name driver-smoke-test --full 1
qm start 999
# wait ~60-90s for first boot, then:
qm agent 999 ping
qm agent 999 network-get-interfaces | grep -A3 '"name" : "eth0"'
qm destroy 999
```

`network-get-interfaces` must return an IPv4 address on the NIC. If `qm agent
999 ping` fails, the agent isn't installed/enabled in the image — go back to
the customize step; this is the single most common cause of the driver
timing out in Rancher.

---

## Optional: template-side setup for Longhorn (or other storage provisioners)

The driver can attach an **extra blank disk** per node
(`--pve-extra-disk-size` + `--pve-extra-disk-storage`), which shows up in the
guest as `/dev/sdb` (boot disk is `sda` when you use the layout above). The
driver deliberately does **not** format or mount it — that belongs to the
guest, and the cleanest place is cloud-init in the template so every clone
sets it up on first boot.

Bake a small cloud-config drop-in into the image:

```bash
cat > 99-longhorn-disk.cfg <<'EOF'
#cloud-config
# Formats the driver's extra disk (if present & unformatted) and mounts it
# for Longhorn. disk_setup/fs_setup run once per instance (i.e. once per
# clone); the fstab entry keeps the mount across reboots.
disk_setup:
  /dev/sdb:
    table_type: gpt
    layout: true
    overwrite: false
fs_setup:
  - label: longhorn
    filesystem: ext4
    device: /dev/sdb
    partition: 1
    overwrite: false
mounts:
  - ["/dev/sdb1", "/var/lib/longhorn", "auto", "defaults,nofail,x-systemd.device-timeout=30s", "0", "2"]
EOF

virt-customize -a <your-image>.qcow2 \
  --mkdir /etc/cloud/cloud.cfg.d \
  --copy-in 99-longhorn-disk.cfg:/etc/cloud/cloud.cfg.d/
```

Then in Rancher, set the machine pool fields `extra-disk-size: 100` and
`extra-disk-storage: local-lvm` (see the
[Rancher setup guide](rancher-setup.md#machine-pool-fields)) and Longhorn will
find its dedicated device already mounted at `/var/lib/longhorn`.

If you instead use raw disk passthrough or Ceph-backed storage classes served
by an external provisioner, you don't need the extra disk at all — skip both
the template drop-in and the driver flags.

## Checklist before moving on

- [ ] `qm agent <test-vmid> ping` succeeds on a manual clone.
- [ ] `network-get-interfaces` shows an IPv4 on the clone's NIC.
- [ ] Template has a cloud-init drive (`ide2 ... cloudinit`).
- [ ] You know which cloud-init user you'll use (`debian` for Debian,
      `rancher` via `ciuser` for Leap Micro) — it becomes `--ssh-user`.
- [ ] `curl`, `bash`, passwordless `sudo` verified inside the image.
- [ ] The template VMID is written down — it is `pve-template-vmid`.
