# Preparing a Proxmox VE template for the `pve` node driver

Every node Rancher creates is a **full clone of one PVE template**, so the
template is where all guest-side requirements are baked in. This guide covers
two supported base images:

- **Debian 13 (trixie) `genericcloud`** — recommended default, classic mutable
  cloud image.
- **openSUSE Leap Micro 6.2 `Default-qcow`** — immutable/transactional OS for
  workloads that want a container-focused, self-healing base.

Both images were verified to contain everything the driver needs. Whichever
you pick, the five hard requirements are:

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
   never reaches `Ready` in Rancher. The driver also uses this account to
   format and mount data disks, so passwordless `sudo` is doubly load-bearing.
4. **An address for the NIC** — either DHCP on the bridge used for `net0`, or a
   static `--pve-ipconfig` you set yourself. See
   [docs/networking.md](networking.md) for a controlled DHCP setup.
5. **The packages your storage stack needs, baked into the image.** The driver
   attaches, formats and mounts data disks itself, but it does not install
   packages: `open-iscsi` and friends have to be in the template. See
   [Guest dependencies for Longhorn](#guest-dependencies-for-longhorn) below.

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
```

`network-get-interfaces` must return an IPv4 address on the NIC. If `qm agent
999 ping` fails, the agent isn't installed/enabled in the image — go back to
the customize step; this is the single most common cause of the driver
timing out in Rancher.

Then check the storage prerequisites (skip if you are not using Longhorn) and
prove that disk serials reach the guest — this is what the driver keys on to
find a data disk, so if it does not work here it will not work in Rancher:

```bash
qm guest exec 999 -- systemctl is-active iscsid
qm guest exec 999 -- /bin/sh -c 'lsmod | grep iscsi_tcp'

# Attach a throwaway 1GB disk with a serial, then look for it the way the
# driver does. The disk is destroyed along with the test VM below.
qm set 999 --scsi1 local-lvm:1,serial=pvedata1
qm guest exec 999 -- lsblk -ndo NAME,SERIAL
# expect a line whose SERIAL column reads pvedata1

qm destroy 999
```

`qm guest exec` needs the guest agent, which requirement 1 already covers. If
the `SERIAL` column is empty, the template is using a disk controller that does
not pass serials through — use `virtio-scsi-single` as in the steps above.

---

## Guest dependencies for Longhorn

The driver attaches each data disk, formats it and mounts it at the path you
gave, before the node joins the cluster. What it does **not** do is install
packages — that is the template's job, and doing it at provisioning time would
make every node build depend on a package mirror.

Longhorn needs the following in the guest:

| Requirement | Why |
|---|---|
| `open-iscsi` installed and `iscsid` enabled | Longhorn attaches every volume over iSCSI to the node |
| `iscsi_tcp` kernel module loaded at boot | the iSCSI initiator transport |
| `nfs-common` (Debian) / `nfs-client` (SUSE) | RWX volumes are served through an NFS share-manager pod |
| `cryptsetup` + `dm_crypt` | only for encrypted volumes |
| a `multipathd` blacklist for Longhorn devices | multipath otherwise claims the device and Longhorn's mount fails |
| `xfsprogs` | only if a data disk uses `fs=xfs` |

### Debian 13

```bash
virt-customize -a debian-13-genericcloud-amd64.qcow2 \
  --install qemu-guest-agent,open-iscsi,nfs-common,cryptsetup,xfsprogs \
  --run-command 'systemctl enable qemu-guest-agent iscsid' \
  --run-command 'echo iscsi_tcp > /etc/modules-load.d/longhorn.conf' \
  --mkdir /etc/multipath/conf.d \
  --run-command 'printf "blacklist {\n    devnode \"^sd[a-z0-9]+\"\n}\n" > /etc/multipath/conf.d/longhorn.conf'
```

This replaces the `--install qemu-guest-agent` call in [step A.1](#a1-download-and-customize-the-image);
run one or the other, not both.

### openSUSE Leap Micro 6.2

Leap Micro is transactional, so packages go in through `transactional-update`
and the change lands in a new snapshot:

```bash
virt-customize -a openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2 \
  --run-command 'transactional-update -n pkg install open-iscsi nfs-client cryptsetup xfsprogs' \
  --run-command 'systemctl enable qemu-guest-agent iscsid' \
  --run-command 'echo iscsi_tcp > /etc/modules-load.d/longhorn.conf'
```

> An alternative for mutable hosts only is Longhorn's own
> `longhorn-iscsi-installation.yaml` and `longhorn-nfs-installation.yaml`
> DaemonSets, which chroot the host and install the packages from inside the
> cluster. They do not work on Leap Micro, and a node joins before they have
> run, so volumes can fail to attach on a fresh node. Baking the packages in
> avoids both problems.

### Why there is no cloud-init disk drop-in any more

Earlier versions of this guide baked a `disk_setup`/`fs_setup`/`mounts`
cloud-config into the image to prepare a single `/dev/sdb`. That is gone: the
driver now does the work itself, keyed on each disk's PVE **serial** rather
than a kernel name, which is the only approach that survives a node having more
than one data disk. If you still have that drop-in in an image, delete it —
two mechanisms formatting the same device is a good way to lose data.

## Checklist before moving on

- [ ] `qm agent <test-vmid> ping` succeeds on a manual clone.
- [ ] `network-get-interfaces` shows an IPv4 on the clone's NIC.
- [ ] Template has a cloud-init drive (`ide2 ... cloudinit`).
- [ ] You know which cloud-init user you'll use (`debian` for Debian,
      `rancher` via `ciuser` for Leap Micro) — it becomes `--ssh-user`.
- [ ] `curl`, `bash`, passwordless `sudo` verified inside the image — the last
      one is what lets the driver format and mount data disks.
- [ ] `lsblk -ndo NAME,SERIAL` on a clone shows the serial of a test disk.
- [ ] If you use Longhorn: `iscsid` active, `iscsi_tcp` loaded, multipath
      blacklist in place.
- [ ] The template VMID is written down — it is `pve-template-vmid`.
