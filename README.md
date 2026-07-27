# pve-rancher-driver

A [Rancher node driver](https://ranchermanager.docs.rancher.com/pages-for-subheaders/node-drivers-and-node-templates)
for [Proxmox VE](https://www.proxmox.com/en/proxmox-virtual-environment/overview),
driver name **`pve`**. It lets an RKE2 or K3s cluster in Rancher provision,
scale and remove Proxmox VE virtual machines as machine-pool nodes by cloning
an existing VM template, booting it, and waiting for the QEMU guest agent to
report the cloned VM's IP. Authentication to PVE is via an API token (no root
password ever lands in a cloud credential).

## How it works

```
Rancher UI   ──┐
                ├─► docker-machine-driver-pve (this binary)
                │       │
PVE REST API ◄──┘       ├─ Resolve target node (or first online node)
                        ├─ Clone template VMID -> new VMID
                        ├─ Apply overrides (cores, sockets, memory, onboot,
                        │   optional cloud-init ipconfig0 / sshkeys)
                        ├─ Start the VM
                        └─ Poll QEMU guest agent for the first IPv4 address
                            → returns "created" once the agent answers
```

Every flag the driver declares in `GetCreateFlags()` becomes a node-template
field in the Rancher UI automatically (no separate UI bundle is needed in
modern Rancher — the `NodeDriver` resource asks the binary for its flags).

## Project layout

```
cmd/docker-machine-driver-pve/   Plugin entrypoint (plugin.RegisterDriver)
pkg/driver/                      libmachine Driver implementation
pkg/proxmox/                    go-proxmox wrapper used by the driver
deploy/nodedriver.yaml           Rancher NodeDriver CRD to apply
Makefile                         build / cross-compile / checksums
```

## Prerequisites

### Proxmox VE API token

Create a dedicated user and a token, then grant a least-privilege role **to
both the user and the token** (PVE tokens do not inherit the user's ACLs
unless `--privsep 0` is set):

```bash
pveum role add RancherPVENode -privs "VM.Clone,VM.Allocate,VM.Audit,VM.PowerMgmt,VM.Config.Disk,VM.Config.CPU,VM.Config.Memory,VM.Config.Network,VM.Config.Cloudinit,VM.Config.Options,VM.Monitor,Datastore.AllocateSpace,Datastore.Audit,SDN.Use,Pool.Allocate"
pveum user add rancher@pve
pveum user token add rancher@pve machine
pveum acl modify / -user rancher@pve -role RancherPVENode
pveum acl modify / -token 'rancher@pve!machine' -role RancherPVENode
```

The driver's `pve-api-token-id` is `rancher@pve!machine`; the
`pve-api-token-secret` is printed once by `pveum user token add` — save it,
it is not shown again.

### VM template

VMs are produced by cloning a template, so the template must be ready before
you point the driver at it:

1. `qemu-guest-agent` must be **baked into** the image (the driver polls it
   the moment the clone boots; installing the agent via a first-boot
   cloud-init script is too late).
2. The VM needs a cloud-init drive (e.g. `--ide2 local-lvm:cloudinit`) if you
   set `--pve-cloudinit`.
3. The cloud-init user (the `--ssh-user` flag, default `root`) needs
   passwordless `sudo` plus `curl` and `bash` on `PATH` so Rancher's
   system-agent bootstrap can finish after `Create` returns.

```bash
# on the PVE host
wget https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img
apt install -y libguestfs-tools
virt-customize -a ubuntu-24.04-server-cloudimg-amd64.img --install qemu-guest-agent

qm create 9000 --name ubuntu-2404-tmpl --memory 2048 --cores 2 \
  --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-single \
  --agent 1 --serial0 socket --vga serial0
qm importdisk 9000 ubuntu-24.04-server-cloudimg-amd64.img local-lvm
qm set 9000 --scsi0 local-lvm:vm-9000-disk-0 --boot order=scsi0 \
  --ide2 local-lvm:cloudinit
qm template 9000
```

## Build

```bash
make build                       # build ./docker-machine-driver-pve
make dist                        # cross-compile darwin/linux amd64+arm64 + checksums.txt
make vet test
```

## Register in Rancher

Apply the bundled manifest against the **Rancher local cluster**:

```bash
kubectl apply -f deploy/nodedriver.yaml
```

First edit `deploy/nodedriver.yaml` and replace `<VERSION>` and `<SHA256>`
with the matching line from `dist/checksums.txt` for
`docker-machine-driver-pve-linux-amd64` (the OS/arch of the host running the
Rancher management plane). `whitelistDomains` already lists all three GitHub
release redirect hosts — don't drop any, or the download silently stalls.

Alternatively, paste the URL and checksum directly through the UI at
**Cluster Management → Drivers → Node Drivers → Add Node Driver**.

## Standalone testing with docker-machine

For local debugging without Rancher, install the plugin on your `PATH` and
invoke it through docker-machine:

```bash
make build && install -m 0755 docker-machine-driver-pve /usr/local/bin/
docker-machine create --driver pve \
  --pve-api-url https://pve.example.com:8006/api2/json \
  --pve-api-token-id rancher@pve!machine \
  --pve-api-token-secret "$(cat token.secret)" \
  --pve-api-insecure \
  --pve-template-vmid 9000 \
  --pve-cores 2 --pve-memory 4096 \
  --pve-cloudinit --pve-ipconfig ip=dhcp \
  --ssh-user ubuntu \
  pve-test-node
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `pve-api-url` | *(required)* | PVE REST API base URL, e.g. `https://host:8006/api2/json` |
| `pve-api-token-id` | *(required)* | PVE API token id (`USER@REALM!TOKENID`) |
| `pve-api-token-secret` | *(required)* | PVE API token secret |
| `pve-api-insecure` | `false` | Skip TLS certificate verification |
| `pve-ca-cert` | *(empty)* | PEM CA certificate content to trust for the PVE API |
| `pve-node` | *(first online)* | Target PVE node name |
| `pve-vmid` | `0` | Explicit VMID for the created VM, `0` = auto-assigned |
| `pve-template-vmid` | *(required)* | Template VMID to clone from |
| `pve-vmname` | machine name | Override the PVE VM name |
| `pve-cores` | `2` | CPU cores per socket |
| `pve-sockets` | `1` | CPU sockets |
| `pve-memory` | `2048` | RAM in MB |
| `pve-disk` | `20` | Disk size in GB (informational; template disk is used as-is) |
| `pve-net-iface` | *(empty)* | Restrict IP discovery to this guest interface name |
| `pve-net-device` | `net0` | PVE config device (`net0`..`net31`) whose MAC pins down IP discovery |
| `pve-agent-timeout` | `300` | Seconds to wait for the QEMU guest agent to report an IP |
| `pve-skip-permission-check` | `false` | Skip the token-permission probe in `PreCreateCheck` |
| `pve-keep-on-failure` | `false` | Leave the cloned VM in place when Create fails (debugging only) |
| `pve-disk` | `20` | Disk size in GB (informational; template disk is used as-is) |
| `pve-cloudinit` | `false` | Push `ipconfig0` / `sshkeys` to the cloned VM |
| `pve-ipconfig` | `ip=dhcp` | Cloud-init `ipconfig0` value |
| `pve-sshkeys` | *(empty)* | Cloud-init `sshkeys` value (URL or inline) |
| `pve-onboot` | `false` | Start the VM automatically on PVE boot |
| `ssh-user` | `root` | SSH user used to log into the VM |
| `ssh-port` | `22` | SSH port used to log into the VM |

## Compatibility

- **Rancher**: v2.x with `management.cattle.io/v3` `NodeDriver` support.
- **Proxmox VE**: 8.x and 9.x, when the API token above is granted the role.

## License

MIT — see `LICENSE`.