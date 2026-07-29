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
   [Adding your own packages to the image](#adding-your-own-packages-to-the-image)
   for the mechanism and [Guest dependencies for Longhorn](#guest-dependencies-for-longhorn)
   for the specific list.

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
  --run-command 'systemctl enable qemu-guest-agent' \
  --truncate /etc/machine-id

# Optional: grow the base disk so nodes have room for container images.
qemu-img resize debian-13-genericcloud-amd64.qcow2 20G
```

> This installs the bare minimum. If the nodes will run Longhorn, or you need
> your own packages, a CA or kernel modules in the image, extend this one
> `virt-customize` call rather than adding a second — see
> [Adding your own packages to the image](#adding-your-own-packages-to-the-image)
> and [Guest dependencies for Longhorn](#guest-dependencies-for-longhorn).

> Use **`genericcloud`**, not `nocloud`: the `nocloud` variant omits
> cloud-init itself. The genericcloud image ships the `debian` user with
> passwordless sudo already configured.

### A.2 Create the template VM

```bash
export TMPL=9000              # VMID for the template
export STORAGE=local-lvm      # PVE storage holding the disks; yours may differ
qm create $TMPL --name debian-13-tmpl --memory 2048 --cores 2 \
  --cpu x86-64-v2-AES \
  --net0 virtio,bridge=vmbr0 \
  --scsihw virtio-scsi-single --agent 1 \
  --serial0 socket --vga serial0 --ostype l26

# Import the image, then attach whatever volume PVE actually created. Do not
# hardcode the volume name: block storages (LVM/ZFS/Ceph) name it
# `vm-9000-disk-0`, while directory storages use `9000/vm-9000-disk-0.qcow2`.
# importdisk registers it as `unused0`, so read it back from the config.
qm importdisk $TMPL debian-13-genericcloud-amd64.qcow2 $STORAGE
qm set $TMPL --scsi0 "$(qm config $TMPL | sed -n 's/^unused0: //p'),discard=on,ssd=1" \
  --boot order=scsi0
qm set $TMPL --ide2 $STORAGE:cloudinit
```

> **`--cpu x86-64-v2-AES` is not cosmetic.** PVE defaults new VMs to `kvm64`,
> which advertises only the 2003-era baseline x86-64 instruction set — no
> SSE4.2, no POPCNT. glibc 2.34+ is compiled for **x86-64-v2** in most modern
> container base images (SUSE BCI, recent Alpine/Debian derivatives), and on a
> `kvm64` guest those images abort at startup with:
>
> ```
> Fatal glibc error: CPU does not support x86-64-v2
> ```
>
> This surfaces as Rancher helm-operation and system-agent pods crash-looping
> on a node that otherwise provisioned perfectly, so it is easy to misread as a
> Rancher problem. `x86-64-v2-AES` is the portable fix: it still allows live
> migration between hosts with different physical CPUs, which `--cpu host` does
> not. Use `host` only if you never migrate, or need the guest to see every
> feature of the physical CPU (AVX-512, nested virt).
>
> **Changing this on an existing template requires a power cycle of each node** —
> the CPU model is fixed at VM start, so a reboot from inside the guest is not
> enough. Existing VMs: `qm stop <vmid>` then `qm start <vmid>`, or just let
> Rancher recreate them.

> **`$TMPL` and `$STORAGE` only live for the current shell.** If you reconnect,
> open a second terminal, or paste these blocks one at a time across sessions,
> they are unset — and `qm set` then fails with `400 not enough arguments`,
> because the VMID it needed expanded to nothing. Check with
> `echo "[$TMPL] [$STORAGE]"`, re-run both `export`s, or use literal values
> (`qm set 9000 --ide2 VM-Storage:cloudinit`). `qm config 9000` shows what
> actually landed on the VM.

Notes:

- `--agent 1` tells PVE to expect the QEMU guest agent (matches requirement 1).
- `--ide2 $STORAGE:cloudinit` attaches the cloud-init drive (requirement 2). The
  storage must have the **Images** content type enabled — `pvesm status` lists
  what each one allows.
- **Storage type changes the volume name**, which is why `--scsi0` reads it back
  from `unused0` instead of naming it. A block storage (LVM-thin, ZFS, Ceph)
  produces `vm-9000-disk-0`; a directory storage produces
  `9000/vm-9000-disk-0.qcow2`. Hardcoding either one fails on the other with
  `unable to parse directory volume name`.
- On PVE 8 and later you can do both steps at once and skip `importdisk`
  entirely, letting PVE name the volume:
  `qm set $TMPL --scsi0 $STORAGE:0,import-from=/abs/path/to/image.qcow2,discard=on,ssd=1`
- `vmbr0` must be a bridge whose network hands out DHCP leases, unless you
  plan to pass static `--pve-ipconfig` per node.
- Do **not** set `--ciuser` for Debian: the image's default `debian` user is
  already correct. Set **VM User** to `debian` on the machine pool — that one
  field drives both the cloud-init user and the SSH login.
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
  --run-command 'systemctl enable qemu-guest-agent' \
  --truncate /etc/machine-id

qemu-img resize openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2 20G
```

> Leap Micro is transactional, so extra packages go in through
> `transactional-update` rather than a plain install — see
> [Adding your own packages to the image](#adding-your-own-packages-to-the-image).

### B.2 Create the template VM

```bash
export TMPL=9001              # same shell-scope caveat as Option A
export STORAGE=local-lvm
qm create $TMPL --name leapmicro-62-tmpl --memory 2048 --cores 2 \
  --cpu x86-64-v2-AES \
  --net0 virtio,bridge=vmbr0 \
  --scsihw virtio-scsi-single --agent 1 \
  --serial0 socket --vga serial0 --ostype l26

qm importdisk $TMPL openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2 $STORAGE
# See Option A: the volume name differs per storage type, so attach the volume
# importdisk registered as `unused0` rather than guessing its name.
qm set $TMPL --scsi0 "$(qm config $TMPL | sed -n 's/^unused0: //p'),discard=on,ssd=1" \
  --boot order=scsi0
qm set $TMPL --ide2 $STORAGE:cloudinit
qm set $TMPL --ciuser rancher
qm template $TMPL
```

The important difference from Debian: **Leap Micro has no default login user**,
so the template sets `--ciuser rancher`. PVE's cloud-init then creates the
`rancher` user (with passwordless sudo) on every clone, applies the
driver-injected SSH keys to it, and clones inherit the `ciuser` setting from the
template. Set **VM User** to `rancher` on the machine pool to match.

The account does not have to pre-exist in the image — cloud-init creates
whatever you name. That is what makes an image with no login user usable at all,
and it means you can pick your own account name here instead of `rancher`.

> Leap Micro is immutable: system packages are managed with
> `transactional-update`. Kubernetes itself runs as RKE2/K3s binaries +
> containers, so nothing extra is needed for the plain Rancher use case — but
> host-level tooling such as Longhorn's iSCSI client must be baked into the
> image, never installed at runtime. See
> [Immutable images (Leap Micro)](#immutable-images-leap-micro).

---

## Verify the template works before pointing Rancher at it

Clone once by hand and confirm the guest agent answers. This section uses the
same two variables, so re-export them if you are in a fresh shell:

```bash
export TMPL=9000              # or 9001 for Leap Micro
export STORAGE=local-lvm      # whatever you used above

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
qm set 999 --scsi1 $STORAGE:1,serial=pvedata1
qm guest exec 999 -- lsblk -ndo NAME,SERIAL
# expect a line whose SERIAL column reads pvedata1

qm destroy 999
```

`qm guest exec` needs the guest agent, which requirement 1 already covers. If
the `SERIAL` column is empty, the template is using a disk controller that does
not pass serials through — use `virtio-scsi-single` as in the steps above.

---

## Adding your own packages to the image

Anything a node needs at the OS level — storage clients, monitoring agents, a
corporate CA, kernel modules — goes into the image **before** it becomes a
template. The driver installs nothing: it clones, configures, attaches disks and
mounts them, and that is deliberate. Installing at provisioning time would make
every node build depend on a reachable package mirror, add a minute or more per
node, and simply not work on an immutable OS like Leap Micro.

The tool is `virt-customize`, from `libguestfs-tools`, run **on the PVE host**
against the downloaded `.qcow2` before `qm importdisk`.

### The general shape

```bash
virt-customize -a <image>.qcow2 \
  --install pkg-one,pkg-two,pkg-three \
  --run-command 'systemctl enable some.service' \
  --run-command 'echo some_module > /etc/modules-load.d/mine.conf' \
  --copy-in ./my-file.conf:/etc/somewhere/ \
  --mkdir /etc/somewhere/else \
  --truncate /etc/machine-id
```

The options you will actually use:

| Option | What it does |
|---|---|
| `--install a,b,c` | Installs packages with the guest's own package manager. Comma-separated, no spaces |
| `--update` | Applies pending updates. Slow, and it makes the image less reproducible — prefer pinning a newer base image |
| `--run-command '<sh>'` | Runs a shell command inside the image |
| `--copy-in <local>:<dir>` | Copies a file or directory in from the host. The **destination is a directory**, not a filename |
| `--mkdir <dir>` | Creates a directory, parents included |
| `--delete <path>` | Removes a path |
| `--root-password password:<pw>` | Sets a root password. Rarely wanted — the driver logs in as the cloud-init user with a key |

### Always end with `--truncate /etc/machine-id`

`virt-customize` writes a fresh, concrete machine ID into the image before it
runs your operations — you will see it in the output:

```
[   4.0] Setting the machine ID in /etc/machine-id
```

For a one-off VM that is correct. For a **template it is exactly wrong**: that
one ID gets baked in, and every clone Rancher creates boots claiming to be the
same host. `machine-id(5)` documents the golden-image rule — the file should be
**empty**, and systemd then generates a unique ID on each first boot. Truncating
it last undoes what the preamble did.

What a shared machine ID actually costs you:

- **DHCP, sometimes.** `systemd-networkd` derives its DHCP DUID from the machine
  ID, so identical IDs mean identical DUIDs and your DHCP server can hand
  several nodes the same lease. `dhclient`, which Debian's cloud images
  generally use, keys on the MAC instead and does not collide — so you may never
  see this symptom on Debian. Do not rely on it.
- **Kubernetes node identity.** `/etc/machine-id` is what a node reports as
  `status.nodeInfo.machineID`. A cluster where every node claims the same ID is
  at best confusing and at worst breaks tooling that assumes uniqueness.
- **journald and host agents**, which use it as the stable host identifier.

Verify without booting anything:

```bash
virt-cat -a <image>.qcow2 /etc/machine-id | wc -c    # expect 0
```

If you already built an image without it, you do not need to start over — run
`virt-customize -a <image>.qcow2 --truncate /etc/machine-id` on its own, any
time before `qm importdisk`.

### Other things that trip people up

- **Operations run in the order you write them.** Put `--install` before the
  `--run-command` that enables the service it just installed, or the enable
  fails against a unit that does not exist yet. This is also why `--truncate
  /etc/machine-id` goes last.
- **`systemctl enable` works; `systemctl start` does not.** Enabling only writes
  symlinks, which is a filesystem operation. Nothing is running inside the
  image, so there is no service to start. Anything that must happen at runtime
  belongs in a systemd unit you enable here.
- **The package manager is non-interactive already.** `--install` handles that
  for you. If you shell out to `apt-get` in a `--run-command`, add `-y` and
  `DEBIAN_FRONTEND=noninteractive` yourself.
- **One command, not five.** Each `virt-customize` invocation boots a small
  appliance to do its work. Combining the operations into a single call is
  noticeably faster and keeps the image build in one reviewable place.

### Kernel modules and sysctls

Modules that must be present at boot go in `/etc/modules-load.d/`, and tunables
in `/etc/sysctl.d/`. Both are read at boot, so neither needs a running system:

```bash
virt-customize -a <image>.qcow2 \
  --run-command 'printf "br_netfilter\noverlay\n" > /etc/modules-load.d/k8s.conf' \
  --run-command 'printf "net.bridge.bridge-nf-call-iptables=1\nnet.ipv4.ip_forward=1\n" > /etc/sysctl.d/99-k8s.conf'
```

RKE2 and K3s set the sysctls they need themselves, so this is only for tuning
beyond their defaults.

### A private CA or an internal registry

```bash
virt-customize -a <image>.qcow2 \
  --copy-in ./corp-ca.crt:/usr/local/share/ca-certificates/ \
  --run-command 'update-ca-certificates'
```

On SUSE the path is `/etc/pki/trust/anchors/` and the command is
`update-ca-certificates` as well.

### Immutable images (Leap Micro)

The same idea, but packages go through `transactional-update`, which applies the
change into a new btrfs snapshot rather than the running root:

```bash
virt-customize -a openSUSE-Leap-Micro.x86_64-Default-qcow.qcow2 \
  --run-command 'transactional-update -n pkg install <packages>' \
  --run-command 'systemctl enable <service>' \
  --truncate /etc/machine-id
```

`/etc` is a writable overlay on Leap Micro, so `--copy-in` to `/etc/...` and
`systemctl enable` behave normally. Everything under `/usr` does not — that is
what `transactional-update` is for.

### Checking what you built, without booting

```bash
virt-ls   -a <image>.qcow2 /usr/sbin/ | grep -E 'qemu-ga|iscsid'
virt-cat  -a <image>.qcow2 /etc/modules-load.d/k8s.conf
virt-cat  -a <image>.qcow2 /etc/os-release
```

Then, once the template exists, the [smoke test](#verify-the-template-works-before-pointing-rancher-at-it)
confirms the same things on a real clone — which is the check that actually
counts, since it exercises the boot path rather than the filesystem.

---

## Guest dependencies for Longhorn

This is the worked example of the section above: the packages Longhorn needs,
and why each one is there.

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
  --run-command 'printf "blacklist {\n    devnode \"^sd[a-z0-9]+\"\n}\n" > /etc/multipath/conf.d/longhorn.conf' \
  --truncate /etc/machine-id
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
  --run-command 'echo iscsi_tcp > /etc/modules-load.d/longhorn.conf' \
  --truncate /etc/machine-id
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
      `rancher` via `ciuser` for Leap Micro) — it becomes `--pve-ssh-user`.
- [ ] `curl`, `bash`, passwordless `sudo` verified inside the image — the last
      one is what lets the driver format and mount data disks.
- [ ] `lsblk -ndo NAME,SERIAL` on a clone shows the serial of a test disk.
- [ ] If you use Longhorn: `iscsid` active, `iscsi_tcp` loaded, multipath
      blacklist in place.
- [ ] `/etc/machine-id` is empty in the image (`virt-cat -a <image> /etc/machine-id | wc -c` = 0),
      so clones do not all share one host identity.
- [ ] The template VMID is written down — it is `pve-template-vmid`.
