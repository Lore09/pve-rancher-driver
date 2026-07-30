// Package driver implements the docker/machine libmachine Driver interface for
// Proxmox VE so it can be registered as a Rancher node driver.
package driver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"

	"github.com/lore09/pve-rancher-driver/pkg/proxmox"
)

// vmNamePrefixPattern is the subset of DNS-label characters a VM name prefix
// may use: it must start and end alphanumeric, with hyphens only in between.
var vmNamePrefixPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

const (
	// maxVMNameLen is the DNS label limit PVE enforces on a VM name.
	maxVMNameLen = 63

	driverName     = "pve"
	defaultSSHUser = "root"
	// defaultAgentTimeout is how long the driver waits for the QEMU guest
	// agent to report a usable IPv4 address on the cloned VM's NIC. The
	// guest agent is the only robust way to learn a DHCP-assigned address
	// without an external IPAM; the default is generous because cold first
	// boots of cloud images can take a couple of minutes.
	defaultAgentTimeout = 5 * time.Minute
	// defaultBootDiskDevice is the PVE config key of the disk grown by
	// --pve-boot-disk-size. scsi0 matches the templates produced by the template
	// preparation guide; templates booting from virtio0/sata0 must override it.
	defaultBootDiskDevice = "scsi0"
	// defaultDiskSetupTimeout bounds the whole SSH-plus-guest-setup phase for
	// data disks. It is separate from the agent timeout because it starts only
	// once an IP is known, and covers mkfs on disks that can be large.
	defaultDiskSetupTimeout = 5 * time.Minute
	defaultNetModel         = "virtio"
)

// Driver is the libmachine Driver implementation backed by Proxmox VE.
type Driver struct {
	*drivers.BaseDriver

	APIUrl           string
	APITokenID       string
	APITokenSecret   string
	Insecure         bool
	CACertPEM        string
	Node             string
	VMID             int
	VMIDRange        string
	TemplateVMID     int
	VMNamePrefix     string
	Cores            int
	Sockets          int
	MemoryMB         int
	BootDiskGB       int
	DataDiskEntries  []string
	DataDisks        []proxmox.DiskSpec
	DiskSetupTimeout time.Duration
	BootDiskDevice   string
	NetIface         string
	NetDevice        string
	NetBridge        string
	NetModel         string
	NetVlanTag       int
	NetMTU           int
	NetFirewall      string
	CloudInit        bool
	IPMode           string
	IPBase           string
	Gateway          string
	Nameservers      string
	SearchDomain     string
	CIUser           string
	SSHKeys          string
	Onboot           bool
	AgentTimeout     time.Duration
	KeepOnFailure    bool
	SkipPermCheck    bool
	NetMAC           string

	client *proxmox.Client
}

// NewDriver returns a fresh Driver instance with default values applied. The
// returned type satisfies drivers.Driver.
func NewDriver(hostName, storePath string) drivers.Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			MachineName: hostName,
			StorePath:   storePath,
			SSHUser:     defaultSSHUser,
			SSHPort:     22,
		},
		Cores:            2,
		Sockets:          1,
		MemoryMB:         2048,
		BootDiskGB:       0,
		BootDiskDevice:   defaultBootDiskDevice,
		NetDevice:        "net0",
		NetModel:         defaultNetModel,
		IPMode:           string(ipModeDHCP),
		Onboot:           false,
		AgentTimeout:     defaultAgentTimeout,
		DiskSetupTimeout: defaultDiskSetupTimeout,
	}
}

// GetCreateFlags declares all the command-line and UI flags the driver
// accepts. The order here defines how fields appear in the Rancher node
// template UI when the driver is registered with its companion UI bundle.
func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			Name:   "pve-api-url",
			EnvVar: "PVE_API_URL",
			Usage:  "Proxmox VE REST API base URL, e.g. https://host:8006/api2/json",
		},
		mcnflag.StringFlag{
			Name:   "pve-api-token-id",
			EnvVar: "PVE_API_TOKEN_ID",
			Usage:  "Proxmox VE API token id, formatted as USER@REALM!TOKENID",
		},
		mcnflag.StringFlag{
			Name:   "pve-api-token-secret",
			EnvVar: "PVE_API_TOKEN_SECRET",
			Usage:  "Proxmox VE API token secret",
		},
		mcnflag.BoolFlag{
			Name:   "pve-api-insecure",
			EnvVar: "PVE_API_INSECURE",
			Usage:  "Disable TLS certificate verification when talking to PVE",
		},
		mcnflag.StringFlag{
			Name:   "pve-ca-cert",
			EnvVar: "PVE_CA_CERT",
			Usage:  "PEM-encoded CA certificate used to verify the PVE API (content, not a path)",
		},
		mcnflag.StringFlag{
			Name:   "pve-node",
			EnvVar: "PVE_NODE",
			Usage:  "Target PVE node name. If empty, the first online node is used",
		},
		mcnflag.IntFlag{
			Name:   "pve-vmid",
			EnvVar: "PVE_VMID",
			Usage:  "Explicit VMID to assign to the created VM. 0 = auto assigned",
			Value:  0,
		},
		mcnflag.StringFlag{
			Name:   "pve-vmid-range",
			EnvVar: "PVE_VMID_RANGE",
			Usage:  "Allocate the VM's ID from this inclusive range, e.g. 200-299. Empty lets Proxmox pick the next free id cluster-wide. Ignored when pve-vmid is set",
		},
		mcnflag.IntFlag{
			Name:   "pve-template-vmid",
			EnvVar: "PVE_TEMPLATE_VMID",
			Usage:  "Existing PVE VM template VMID used to clone new VMs from",
			Value:  0,
		},
		mcnflag.StringFlag{
			Name:   "pve-vm-name-prefix",
			EnvVar: "PVE_VM_NAME_PREFIX",
			Usage:  "Prefix prepended to the PVE VM name as <prefix>-<machine name>. Empty uses the machine name unchanged. Letters, digits and inner hyphens only — PVE validates the result as a DNS name",
		},
		mcnflag.IntFlag{
			Name:   "pve-cores",
			EnvVar: "PVE_CORES",
			Usage:  "Number of CPU cores per socket",
			Value:  2,
		},
		mcnflag.IntFlag{
			Name:   "pve-sockets",
			EnvVar: "PVE_SOCKETS",
			Usage:  "Number of CPU sockets",
			Value:  1,
		},
		mcnflag.IntFlag{
			Name:   "pve-memory",
			EnvVar: "PVE_MEMORY",
			Usage:  "Amount of RAM in MB",
			Value:  2048,
		},
		mcnflag.IntFlag{
			Name:   "pve-boot-disk-size",
			EnvVar: "PVE_BOOT_DISK_SIZE",
			Usage:  "Grow the cloned boot disk to this size in GB (0 = keep the template's size). PVE can only grow a disk, never shrink it",
			Value:  0,
		},
		mcnflag.StringFlag{
			Name:   "pve-boot-disk-device",
			EnvVar: "PVE_BOOT_DISK_DEVICE",
			Usage:  "PVE config key of the boot disk grown by pve-boot-disk-size, e.g. scsi0, virtio0, sata0",
			Value:  defaultBootDiskDevice,
		},
		mcnflag.StringSliceFlag{
			Name:   "pve-data-disk",
			EnvVar: "PVE_DATA_DISK",
			Usage:  "Data disk to attach, repeatable. Comma-separated key=value pairs: size=<GB>,storage=<pve-storage>[,fs=ext4|xfs|none][,mount=<abs path>][,label=<name>][,device=scsi1..scsi30][,discard=on|off][,iothread=0|1][,backup=0|1]. Unless fs=none, the driver formats the disk and mounts it at mount= over SSH once the VM is up",
		},
		mcnflag.IntFlag{
			Name:   "pve-disk-setup-timeout",
			EnvVar: "PVE_DISK_SETUP_TIMEOUT",
			Usage:  "Seconds to wait for SSH plus formatting and mounting of the data disks",
			Value:  int(defaultDiskSetupTimeout / time.Second),
		},
		mcnflag.StringFlag{
			Name:   "pve-net-iface",
			EnvVar: "PVE_NET_IFACE",
			Usage:  "Name of the guest interface used to discover the machine IP",
		},
		mcnflag.StringFlag{
			Name:   "pve-net-device",
			EnvVar: "PVE_NET_DEVICE",
			Usage:  "PVE config device (net0..net31) whose MAC is used for IP discovery, and which the pve-net-* settings below are written to",
			Value:  "net0",
		},
		mcnflag.StringFlag{
			Name:   "pve-net-bridge",
			EnvVar: "PVE_NET_BRIDGE",
			Usage:  "PVE bridge to attach the NIC to, e.g. vmbr1. Leave empty to inherit the template's network configuration untouched",
		},
		mcnflag.StringFlag{
			Name:   "pve-net-model",
			EnvVar: "PVE_NET_MODEL",
			Usage:  "Emulated NIC model used when pve-net-bridge is set (virtio, e1000, rtl8139, ...)",
			Value:  defaultNetModel,
		},
		mcnflag.IntFlag{
			Name:   "pve-net-vlan-tag",
			EnvVar: "PVE_NET_VLAN_TAG",
			Usage:  "802.1Q VLAN tag for the NIC (0 = untagged). Requires pve-net-bridge",
			Value:  0,
		},
		mcnflag.IntFlag{
			Name:   "pve-net-mtu",
			EnvVar: "PVE_NET_MTU",
			Usage:  "NIC MTU (0 = PVE default). Requires pve-net-bridge",
			Value:  0,
		},
		mcnflag.StringFlag{
			Name:   "pve-net-firewall",
			EnvVar: "PVE_NET_FIREWALL",
			Usage:  "Enable the PVE firewall on the NIC: true or false. Empty leaves the PVE default. Requires pve-net-bridge",
		},
		mcnflag.IntFlag{
			Name:   "pve-agent-timeout",
			EnvVar: "PVE_AGENT_TIMEOUT",
			Usage:  "Seconds to wait for the QEMU guest agent to report the VM's IP",
			Value:  int(defaultAgentTimeout / time.Second),
		},
		mcnflag.BoolFlag{
			Name:   "pve-cloudinit",
			EnvVar: "PVE_CLOUDINIT",
			Usage:  "Deprecated and always on. Cloud-init delivers the SSH key, the address and the DNS settings, so the driver enables it regardless of this flag",
		},
		mcnflag.StringFlag{
			Name:   "pve-ip-mode",
			EnvVar: "PVE_IP_MODE",
			Usage:  "How the machine gets its address: dhcp (default) or static. static requires --pve-ip-base, --pve-gateway and --pve-vmid-range",
			Value:  string(ipModeDHCP),
		},
		mcnflag.StringFlag{
			Name:   "pve-ip-base",
			EnvVar: "PVE_IP_BASE",
			Usage:  "First address of the static pool, in CIDR form, e.g. 10.10.20.10/24. The machine with the lowest VMID in --pve-vmid-range gets this address; each later VMID gets the next one",
		},
		mcnflag.StringFlag{
			Name:   "pve-gateway",
			EnvVar: "PVE_GATEWAY",
			Usage:  "Default gateway for static addressing, e.g. 10.10.20.1. Must be inside the subnet of --pve-ip-base",
		},
		mcnflag.StringFlag{
			Name:   "pve-nameservers",
			EnvVar: "PVE_NAMESERVERS",
			Usage:  "DNS servers, space- or comma-separated. Applies in both dhcp and static mode; leave empty to keep the resolver DHCP supplies",
		},
		mcnflag.StringFlag{
			Name:   "pve-searchdomain",
			EnvVar: "PVE_SEARCHDOMAIN",
			Usage:  "DNS search domain, e.g. cluster.lan. Applies in both dhcp and static mode",
		},
		mcnflag.StringFlag{
			Name:   "pve-ciuser",
			EnvVar: "PVE_CIUSER",
			Usage:  "Cloud-init user PVE should create/configure with the SSH keys (required for images without a default user, e.g. openSUSE Leap Micro)",
		},
		mcnflag.StringFlag{
			Name:   "pve-sshkeys",
			EnvVar: "PVE_SSHKEYS",
			Usage:  "Extra OpenSSH public keys injected via cloud-init (one per line, plain text). The driver's own generated key is always added",
		},
		mcnflag.BoolFlag{
			Name:   "pve-onboot",
			EnvVar: "PVE_ONBOOT",
			Usage:  "Whether the VM should autostart on PVE boot",
		},
		mcnflag.BoolFlag{
			Name:   "pve-skip-permission-check",
			EnvVar: "PVE_SKIP_PERMISSION_CHECK",
			Usage:  "Skip the PreCreateCheck probe of the API token's effective privileges",
		},
		mcnflag.BoolFlag{
			Name:   "pve-keep-on-failure",
			EnvVar: "PVE_KEEP_ON_FAILURE",
			Usage:  "Leave the cloned VM in place when Create fails (standalone CLI debugging only)",
		},
		// These MUST keep the pve- prefix. Rancher derives a machine-config
		// field name by splitting a flag on its first dash and discarding what
		// precedes it, assuming that part is the driver name — so a flag named
		// `ssh-port` becomes the field `port`, and Rancher then passes it back
		// as `--pve-port`, which this driver does not define. Provisioning fails
		// with "flag provided but not defined: -pve-port".
		mcnflag.StringFlag{
			Name:   "pve-ssh-user",
			EnvVar: "PVE_SSH_USER",
			Usage:  "SSH user used to log into the VM. Must be the account that exists in the guest (debian, rancher, ...)",
			Value:  defaultSSHUser,
		},
		mcnflag.IntFlag{
			Name:   "pve-ssh-port",
			EnvVar: "PVE_SSH_PORT",
			Usage:  "SSH port used to log into the VM",
			Value:  22,
		},
	}
}

// SetConfigFromFlags is invoked once all flags have been parsed. It copies the
// parsed flag values into the driver fields that Create/Start/etc rely on.
func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.APIUrl = flags.String("pve-api-url")
	d.APITokenID = flags.String("pve-api-token-id")
	d.APITokenSecret = flags.String("pve-api-token-secret")
	d.Insecure = flags.Bool("pve-api-insecure")
	d.CACertPEM = flags.String("pve-ca-cert")
	d.Node = flags.String("pve-node")
	d.VMID = flags.Int("pve-vmid")
	d.VMIDRange = strings.TrimSpace(flags.String("pve-vmid-range"))
	d.TemplateVMID = flags.Int("pve-template-vmid")
	d.VMNamePrefix = strings.TrimSpace(flags.String("pve-vm-name-prefix"))
	d.Cores = flags.Int("pve-cores")
	d.Sockets = flags.Int("pve-sockets")
	d.MemoryMB = flags.Int("pve-memory")
	d.BootDiskGB = flags.Int("pve-boot-disk-size")
	d.BootDiskDevice = flags.String("pve-boot-disk-device")
	if d.BootDiskDevice == "" {
		d.BootDiskDevice = defaultBootDiskDevice
	}
	d.DataDiskEntries = flags.StringSlice("pve-data-disk")
	d.NetIface = flags.String("pve-net-iface")
	d.NetDevice = flags.String("pve-net-device")
	if d.NetDevice == "" {
		d.NetDevice = "net0"
	}
	d.NetBridge = flags.String("pve-net-bridge")
	d.NetModel = flags.String("pve-net-model")
	d.NetVlanTag = flags.Int("pve-net-vlan-tag")
	d.NetMTU = flags.Int("pve-net-mtu")
	d.NetFirewall = strings.TrimSpace(flags.String("pve-net-firewall"))
	// Cloud-init is not optional. It is the only channel that delivers the SSH
	// key the driver needs to format data disks and that Rancher's system-agent
	// needs to bootstrap the node, plus ipconfig0 for static addressing and the
	// nameserver/searchdomain options. A node provisioned without it comes up
	// and then never reaches Ready, which reads as a Rancher fault.
	//
	// The flag stays declared rather than being deleted: removing it would drop
	// the field from the generated CRD schema while existing machine pools still
	// carry it, and Rancher would then pass --pve-cloudinit to a driver that no
	// longer defines it ("flag provided but not defined").
	if !flags.Bool("pve-cloudinit") {
		log.Warnf("pve: --pve-cloudinit was not set; enabling it anyway because the driver cannot provision a working node without it")
	}
	d.CloudInit = true
	d.IPMode = strings.TrimSpace(flags.String("pve-ip-mode"))
	d.IPBase = strings.TrimSpace(flags.String("pve-ip-base"))
	d.Gateway = strings.TrimSpace(flags.String("pve-gateway"))
	d.Nameservers = strings.TrimSpace(flags.String("pve-nameservers"))
	d.SearchDomain = strings.TrimSpace(flags.String("pve-searchdomain"))
	d.CIUser = flags.String("pve-ciuser")
	d.SSHKeys = flags.String("pve-sshkeys")
	d.Onboot = flags.Bool("pve-onboot")
	d.SkipPermCheck = flags.Bool("pve-skip-permission-check")
	d.KeepOnFailure = flags.Bool("pve-keep-on-failure")
	timeoutSec := flags.Int("pve-agent-timeout")
	if timeoutSec > 0 {
		d.AgentTimeout = time.Duration(timeoutSec) * time.Second
	} else {
		d.AgentTimeout = defaultAgentTimeout
	}
	if setupSec := flags.Int("pve-disk-setup-timeout"); setupSec > 0 {
		d.DiskSetupTimeout = time.Duration(setupSec) * time.Second
	} else {
		d.DiskSetupTimeout = defaultDiskSetupTimeout
	}
	d.SSHUser = flags.String("pve-ssh-user")
	d.SSHPort = flags.Int("pve-ssh-port")
	d.SetSwarmConfigFromFlags(flags)
	return nil
}

// DriverName returns the canonical driver identifier used by docker-machine.
func (d *Driver) DriverName() string { return driverName }

// PreCreateCheck validates that the minimal config is present before Rancher
// attempts to provision, then probes the API token's effective privileges
// against the live PVE major version. The probe is skippable for environments
// with read-restricted /access/permissions.
func (d *Driver) PreCreateCheck() error {
	if d.APIUrl == "" {
		return errors.New("pve: --pve-api-url is required")
	}
	if d.APITokenID == "" {
		return errors.New("pve: --pve-api-token-id is required")
	}
	if d.APITokenSecret == "" {
		return errors.New("pve: --pve-api-token-secret is required")
	}
	if d.TemplateVMID == 0 {
		return errors.New("pve: --pve-template-vmid is required to clone a VM")
	}
	if err := d.validateVMNamePrefix(); err != nil {
		return err
	}
	if _, _, err := parseVMIDRange(d.VMIDRange); err != nil {
		return err
	}
	if d.VMIDRange != "" && d.VMID != 0 {
		return fmt.Errorf("pve: --pve-vmid %d and --pve-vmid-range %q are mutually exclusive; an explicit id makes the range meaningless (and cannot work for a pool of more than one machine)", d.VMID, d.VMIDRange)
	}
	if err := d.validateAddressing(); err != nil {
		return err
	}
	disks, err := ParseDataDisks(d.DataDiskEntries)
	if err != nil {
		return err
	}
	d.DataDisks = disks
	// Formatting and mounting happens over SSH with the keypair cloud-init
	// injects, so a mounting disk without cloud-init could never be set up.
	// Failing here beats cloning a VM the driver cannot finish.
	if !d.CloudInit {
		for _, disk := range disks {
			if disk.NeedsGuestSetup() {
				return fmt.Errorf("pve: --pve-cloudinit is required to format and mount %s: the driver needs the injected SSH key to reach the guest (use fs=none to attach the disk raw instead)", disk.Mount)
			}
		}
	}
	if _, ferr := parseNetFirewall(d.NetFirewall); ferr != nil {
		return ferr
	}
	// The pve-net-* knobs are only ever applied as part of rewriting the net
	// device, which is gated on a bridge being named. Silently ignoring them
	// would look like the driver honoured a VLAN it never set, so fail loudly.
	if d.NetBridge == "" {
		var orphans []string
		if d.NetVlanTag > 0 {
			orphans = append(orphans, "--pve-net-vlan-tag")
		}
		if d.NetMTU > 0 {
			orphans = append(orphans, "--pve-net-mtu")
		}
		if d.NetFirewall != "" {
			orphans = append(orphans, "--pve-net-firewall")
		}
		if len(orphans) > 0 {
			return fmt.Errorf("pve: %s require --pve-net-bridge to be set", strings.Join(orphans, ", "))
		}
	}
	if err := d.init(); err != nil {
		return err
	}
	if d.SkipPermCheck {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return d.client.VerifyPermissions(ctx)
}

func (d *Driver) init() error {
	if d.client != nil {
		return nil
	}
	client, err := proxmox.New(proxmox.Config{
		APIUrl:         d.APIUrl,
		APITokenID:     d.APITokenID,
		APITokenSecret: d.APITokenSecret,
		Insecure:       d.Insecure,
		CACertPEM:      d.CACertPEM,
		Node:           d.Node,
	})
	if err != nil {
		return err
	}
	d.client = client
	return nil
}

// Create provisions a new VM by cloning a configured template, applying
// overrides (CPU/memory/cloud-init) and finally booting it. The clone is
// wrapped in a rollback: any error from Configure or Start removes the
// half-built VM unless --pve-keep-on-failure is set, so a failed Create never
// leaves an orphan.
func (d *Driver) Create() error {
	log.Infof("pve: creating VM %q on Proxmox VE", d.MachineName)
	if err := d.init(); err != nil {
		return err
	}
	if d.TemplateVMID == 0 {
		return errors.New("pve: --pve-template-vmid is required to clone a VM")
	}
	ctx := context.Background()

	vmName := d.resolveVMName()

	// docker-machine/libmachine expects a keypair at GetSSHKeyPath() to log
	// in after Create; the driver is responsible for creating it and for
	// getting the public half onto the guest (via cloud-init below).
	if err := d.ensureSSHKeyPair(); err != nil {
		return err
	}

	assigned, err := d.cloneFromTemplate(ctx, vmName)
	if err != nil {
		return err
	}
	d.VMID = assigned
	log.Infof("pve: cloned VM %d from template %d", assigned, d.TemplateVMID)

	if err := d.finalizeCreate(ctx, vmName); err != nil {
		if !d.KeepOnFailure {
			log.Warnf("pve: Create failed (%v); removing VM %d to avoid orphans", err, d.VMID)
			if rmErr := d.client.Remove(ctx, d.VMID); rmErr != nil {
				log.Warnf("pve: cleanup of VM %d failed: %v", d.VMID, rmErr)
			}
		}
		return err
	}
	return nil
}

// finalizeCreate runs the post-clone steps (Configure + data disks + start +
// NIC MAC capture used by IP discovery + guest-side disk setup). Broken out so
// the rollback in Create covers every failure mode with one cleanup path.
func (d *Driver) finalizeCreate(ctx context.Context, vmName string) error {
	onboot := d.Onboot
	ipConfig, err := d.resolveIPConfig()
	if err != nil {
		return err
	}
	firewall, err := parseNetFirewall(d.NetFirewall)
	if err != nil {
		return err
	}
	opts := proxmox.VMOptions{
		Name:         vmName,
		Cores:        uint16(d.Cores),
		Sockets:      uint16(d.Sockets),
		Memory:       uint32(d.MemoryMB),
		Onboot:       &onboot,
		CloudInit:    d.CloudInit,
		IPConfig:     ipConfig,
		Nameserver:   d.Nameservers,
		SearchDomain: d.SearchDomain,
		CIUser:       d.CIUser,
		NetDevice:    d.NetDevice,
		NetBridge:    d.NetBridge,
		NetModel:     d.NetModel,
		NetVlanTag:   d.NetVlanTag,
		NetMTU:       d.NetMTU,
		NetFirewall:  firewall,
	}
	opts.CIUser = d.resolveCIUser()
	if d.CloudInit {
		keys, err := d.combinedSSHKeys()
		if err != nil {
			return err
		}
		if keys != "" {
			opts.SSHKeys = keys
		}
	} else {
		log.Warnf("pve: cloud-init disabled; no SSH key will be injected — the template must already trust %s", d.GetSSHKeyPath()+".pub")
	}
	if err := d.client.Configure(ctx, d.VMID, opts); err != nil {
		return err
	}
	if ipConfig != "ip=dhcp" {
		log.Infof("pve: VM %d configured with static %s", d.VMID, ipConfig)
	}

	// Grow the boot disk before AddDisks: slot allocation scans the live config
	// for free scsi<N> keys, so resizing the existing boot disk first keeps that
	// scan honest.
	if d.BootDiskGB > 0 {
		if err := d.client.ResizeBootDisk(ctx, d.VMID, d.BootDiskDevice, d.BootDiskGB); err != nil {
			return err
		}
		log.Infof("pve: grew boot disk %s of VM %d to %dGB", d.BootDiskDevice, d.VMID, d.BootDiskGB)
	}

	attached, err := d.client.AddDisks(ctx, d.VMID, d.DataDisks)
	if err != nil {
		return err
	}
	for _, disk := range attached {
		log.Infof("pve: attached %dGB data disk as %s on storage %q (serial %s)",
			disk.Spec.Size, disk.Device, disk.Spec.Storage, disk.Spec.Label)
	}

	if err := d.client.Start(ctx, d.VMID); err != nil {
		return err
	}
	log.Infof("pve: started VM %d", d.VMID)

	// Must stay after Configure: rewriting net<N> for --pve-net-bridge makes
	// PVE mint a new MAC, so reading it any earlier would pin IP discovery to
	// the template's old address.
	mac, err := d.client.VMNetMAC(ctx, d.VMID, d.NetDevice)
	if err != nil {
		log.Warnf("pve: could not read %s MAC for IP discovery: %v; falling back to first-IPv4 detection", d.NetDevice, err)
	} else {
		d.NetMAC = mac
		log.Infof("pve: VM %d uses MAC %s on %s", d.VMID, mac, d.NetDevice)
	}

	// Data disks are set up over SSH, so the address has to be resolved here
	// rather than left to the GetIP call libmachine makes after Create.
	if len(d.DataDisks) > 0 {
		ip, err := d.waitUntilGuestIP(ctx)
		if err != nil {
			return err
		}
		d.IPAddress = ip
		if err := d.setupGuestDisks(attached); err != nil {
			return err
		}
	}
	return nil
}

// vmidClaimAttempts bounds the retry loop below. Each attempt costs one
// /cluster/resources call plus a failed clone, so a handful is plenty: the
// window being retried is the few hundred milliseconds between picking an id
// and Proxmox writing the config file.
const vmidClaimAttempts = 5

// cloneFromTemplate clones the template into a VMID chosen according to the
// pve-vmid / pve-vmid-range settings.
//
// With no range configured this is a straight passthrough: Proxmox picks via
// /cluster/nextid. With a range, the driver picks the lowest free id itself —
// and because "free" is only true until someone else takes it, a pool creating
// several machines at once will occasionally lose the race. That case is
// retried rather than surfaced, since it is expected rather than exceptional.
func (d *Driver) cloneFromTemplate(ctx context.Context, vmName string) (int, error) {
	minID, maxID, err := parseVMIDRange(d.VMIDRange)
	if err != nil {
		return 0, err
	}
	if d.VMID != 0 || d.VMIDRange == "" {
		return d.client.CloneFromTemplate(ctx, d.TemplateVMID, d.VMID, vmName)
	}

	var lastErr error
	for attempt := 1; attempt <= vmidClaimAttempts; attempt++ {
		id, err := d.client.NextFreeVMID(ctx, minID, maxID)
		if err != nil {
			return 0, err
		}
		assigned, err := d.client.CloneFromTemplate(ctx, d.TemplateVMID, id, vmName)
		if err == nil {
			return assigned, nil
		}
		if !isVMIDTakenError(err) {
			return 0, err
		}
		lastErr = err
		log.Warnf("pve: VMID %d was claimed by someone else between selection and clone (attempt %d/%d); picking another",
			id, attempt, vmidClaimAttempts)
	}
	return 0, fmt.Errorf("pve: could not claim a free VMID in %s after %d attempts: %w", d.VMIDRange, vmidClaimAttempts, lastErr)
}

// isVMIDTakenError reports whether a clone failed purely because the chosen
// VMID was taken in the meantime.
//
// This matches on Proxmox's error text, which is fragile — but the alternative
// is retrying every clone failure, which would turn a genuine error (missing
// template, no space on the storage) into five identical ones.
func isVMIDTakenError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// resolveCIUser returns the account cloud-init should create and configure.
//
// The cloud-init user and the SSH user are the same account by definition:
// cloud-init installs the SSH keys for `ciuser`, and that is the only account
// the driver or Rancher can subsequently log into. Deriving one from the other
// removes a whole class of "provisions fine, never reaches Ready" failures
// caused by the two fields drifting apart.
func (d *Driver) resolveCIUser() string {
	if ciuser := strings.TrimSpace(d.CIUser); ciuser != "" {
		return ciuser
	}
	return d.SSHUser
}

// resolveVMName builds the name given to the PVE VM: the machine name Rancher
// generated, optionally prefixed. The prefix is deliberately not a full
// override — every node in a machine pool would otherwise land in PVE under
// the same name, distinguishable only by VMID.
func (d *Driver) resolveVMName() string {
	prefix := strings.TrimSpace(d.VMNamePrefix)
	if prefix == "" {
		return d.MachineName
	}
	return prefix + "-" + d.MachineName
}

// validateVMNamePrefix rejects a prefix that would produce a VM name PVE
// refuses. PVE validates the name as a DNS name, so the usable character set is
// narrower than it looks: no underscores, no dots, no leading or trailing
// hyphen, and 63 characters for the whole label.
func (d *Driver) validateVMNamePrefix() error {
	prefix := strings.TrimSpace(d.VMNamePrefix)
	if prefix == "" {
		return nil
	}
	if !vmNamePrefixPattern.MatchString(prefix) {
		return fmt.Errorf("pve: --pve-vm-name-prefix %q must contain only letters, digits and inner hyphens (PVE validates the VM name as a DNS name)", prefix)
	}
	if n := len(d.resolveVMName()); n > maxVMNameLen {
		return fmt.Errorf("pve: --pve-vm-name-prefix %q makes the VM name %d characters long, over the %d-character DNS label limit; use a shorter prefix", prefix, n, maxVMNameLen)
	}
	return nil
}

// parseNetFirewall converts the tri-state --pve-net-firewall flag into a
// pointer: nil means "leave the PVE default alone". A bool flag cannot express
// that third state, and defaulting to false would silently disable a firewall
// the template had enabled.
func parseNetFirewall(s string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return nil, nil
	case "true", "1", "yes", "on":
		v := true
		return &v, nil
	case "false", "0", "no", "off":
		v := false
		return &v, nil
	}
	return nil, fmt.Errorf("pve: --pve-net-firewall must be true or false, got %q", s)
}

// ensureSSHKeyPair creates the docker-machine SSH keypair at GetSSHKeyPath()
// if it does not exist yet. GenerateSSHKey is a no-op when the private key is
// already on disk, so this is safe to call on every Create.
func (d *Driver) ensureSSHKeyPair() error {
	keyPath := d.GetSSHKeyPath()
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return fmt.Errorf("pve: cannot create machine key directory: %w", err)
	}
	if err := ssh.GenerateSSHKey(keyPath); err != nil {
		return fmt.Errorf("pve: cannot generate SSH keypair: %w", err)
	}
	return nil
}

// combinedSSHKeys merges any operator-supplied --pve-sshkeys with the public
// key docker-machine generated for this machine and returns the URL-encoded
// value PVE expects for the `sshkeys` config option.
func (d *Driver) combinedSSHKeys() (string, error) {
	pub, err := os.ReadFile(d.GetSSHKeyPath() + ".pub")
	if err != nil {
		return "", fmt.Errorf("pve: cannot read generated SSH public key: %w", err)
	}
	lines := []string{strings.TrimSpace(string(pub))}
	for _, k := range strings.Split(d.SSHKeys, "\n") {
		if k = strings.TrimSpace(k); k != "" {
			lines = append(lines, k)
		}
	}
	return pveURLEncode(strings.Join(lines, "\n")), nil
}

// pveURLEncode percent-encodes a value for a PVE option whose format is
// `urlencoded`, such as `sshkeys`.
//
// url.QueryEscape is *form* encoding and emits "+" for a space. PVE validates
// with
//
//	$text !~ m/^[-%a-zA-Z0-9_.!~*'()]*$/  ->  "invalid urlencoded string"
//
// and "+" is not in that set, so an SSH key — which always contains spaces,
// between the algorithm, the key body and the comment — is rejected outright.
// Percent-encoding the space instead keeps the output inside the permitted
// character set, since QueryEscape already escapes everything except
// [A-Za-z0-9-_.~].
func pveURLEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// Start powers on the VM if it is not already running.
func (d *Driver) Start() error {
	if err := d.init(); err != nil {
		return err
	}
	st, err := d.GetState()
	if err != nil {
		return err
	}
	if st == state.Running {
		return nil
	}
	return d.client.Start(context.Background(), d.VMID)
}

// Stop performs a graceful shutdown.
func (d *Driver) Stop() error {
	if err := d.init(); err != nil {
		return err
	}
	return d.client.Stop(context.Background(), d.VMID)
}

// Kill force-powers-off the VM.
func (d *Driver) Kill() error {
	if err := d.init(); err != nil {
		return err
	}
	return d.client.Kill(context.Background(), d.VMID)
}

// Restart gracefully reboots the guest.
func (d *Driver) Restart() error {
	if err := d.init(); err != nil {
		return err
	}
	return d.client.Restart(context.Background(), d.VMID)
}

// Remove destroys the VM and its disks.
//
// PVE refuses to destroy a running VM, and Rancher deletes a machine without
// stopping it first — so without the force-stop below, deleting a cluster took
// away every Rancher resource and left the VMs running in Proxmox, with
// nothing left pointing at them.
//
// The stop is a hard power-off rather than a graceful shutdown: the guest is
// about to be deleted, and waiting on an ACPI shutdown that an unhealthy node
// may never honour would just stall the deletion.
func (d *Driver) Remove() error {
	if err := d.init(); err != nil {
		return err
	}
	if d.VMID == 0 {
		// Create failed before a VMID was assigned; there is nothing to remove.
		return nil
	}
	ctx := context.Background()

	switch st, err := d.GetState(); {
	case err != nil:
		// Most likely already gone. Fall through to Remove, which treats a
		// missing VM as success.
		log.Warnf("pve: could not read state of VM %d before removal: %v", d.VMID, err)
	case st != state.Stopped:
		log.Infof("pve: stopping VM %d before removal", d.VMID)
		if err := d.client.Kill(ctx, d.VMID); err != nil {
			log.Warnf("pve: force-stop of VM %d failed: %v; attempting removal anyway", d.VMID, err)
		}
	}

	return d.client.Remove(ctx, d.VMID)
}

// GetState returns the current VM state.
func (d *Driver) GetState() (state.State, error) {
	if err := d.init(); err != nil {
		return state.Error, err
	}
	st, err := d.client.State(context.Background(), d.VMID)
	if err != nil {
		return state.Error, err
	}
	return pveStatusToState(st), nil
}

// pveStatusToState maps PVE's QEMU status vocabulary onto libmachine's state
// enum.
//
// The two do not share a vocabulary and, crucially, do not share a case: PVE
// reports "running", libmachine's state.Running stringifies to "Running".
// Comparing the raw strings therefore never matches, GetState reports
// state.Error for a perfectly healthy VM, and libmachine's post-create
// "Waiting for machine to be running" spins until it gives up with
// "Maximum number of retries (60) exceeded".
func pveStatusToState(status string) state.State {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return state.Running
	case "paused", "suspended":
		return state.Paused
	case "stopped":
		return state.Stopped
	case "prelaunch":
		// Briefly reported between `qm start` and the guest actually running.
		return state.Starting
	default:
		return state.Error
	}
}

// GetURL returns the docker host URL. docker-machine uses this to identify the
// remote docker daemon; we surface the IP via SSH since PVE driver does not
// configure TLS for the docker socket.
func (d *Driver) GetURL() (string, error) {
	if err := d.init(); err != nil {
		return "", err
	}
	ip, err := d.waitUntilGuestIP(context.Background())
	if err != nil {
		return "", err
	}
	d.IPAddress = ip
	if ip == "" {
		return "", nil
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

// GetIP returns the IP address of the VM as reported by the guest agent.
func (d *Driver) GetIP() (string, error) {
	if err := d.init(); err != nil {
		return "", err
	}
	st, err := d.GetState()
	if err != nil {
		return "", err
	}
	if st != state.Running {
		return "", drivers.ErrHostIsNotRunning
	}
	return d.waitUntilGuestIP(context.Background())
}

// GetSSHHostname returns the host docker-machine should SSH into.
func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

// waitUntilGuestIP polls the QEMU guest-agent until an IPv4 address is
// available or the configured agent timeout elapses. If the VM's NIC MAC was
// captured at clone time, IP discovery is restricted to that interface — this
// avoids the well-known failure mode where the guest agent reports IPs on
// Docker/CNI bridges or IPv6 link-locals before DHCP has finished on the NIC
// we actually cloned for.
func (d *Driver) waitUntilGuestIP(ctx context.Context) (string, error) {
	deadline := time.Now().Add(d.AgentTimeout)
	for {
		var (
			ip  string
			err error
		)
		if d.NetMAC != "" {
			ip, err = d.client.GuestIPByMAC(ctx, d.VMID, d.NetMAC)
		} else {
			ip, err = d.client.GuestIP(ctx, d.VMID, d.NetIface)
		}
		if err == nil && ip != "" {
			return ip, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", fmt.Errorf("pve: timed out waiting for guest agent IP: %w", err)
			}
			return "", errors.New("pve: timed out waiting for guest agent IP")
		}
		time.Sleep(3 * time.Second)
	}
}
