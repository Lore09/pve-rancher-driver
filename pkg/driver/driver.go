// Package driver implements the docker/machine libmachine Driver interface for
// Proxmox VE so it can be registered as a Rancher node driver.
package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/state"

	"github.com/lore09/pve-rancher-driver/pkg/proxmox"
)

const (
	driverName    = "pve"
	defaultSSHUser = "root"
	// defaultAgentTimeout is how long the driver waits for the QEMU guest
	// agent to report a usable IPv4 address on the cloned VM's NIC. The
	// guest agent is the only robust way to learn a DHCP-assigned address
	// without an external IPAM; the default is generous because cold first
	// boots of cloud images can take a couple of minutes.
	defaultAgentTimeout = 5 * time.Minute
)

// Driver is the libmachine Driver implementation backed by Proxmox VE.
type Driver struct {
	*drivers.BaseDriver

	APIUrl         string
	APITokenID     string
	APITokenSecret string
	Insecure       bool
	CACertPEM      string
	Node           string
	VMID           int
	TemplateVMID   int
	VMName         string
	Cores          int
	Sockets        int
	MemoryMB       int
	DiskGB         int
	NetIface       string
	NetDevice      string
	CloudInit      bool
	IPConfig       string
	SSHKeys        string
	Onboot         bool
	AgentTimeout   time.Duration
	KeepOnFailure   bool
	SkipPermCheck   bool
	NetMAC          string

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
		VMName:       hostName,
		Cores:        2,
		Sockets:      1,
		MemoryMB:     2048,
		DiskGB:       20,
		NetDevice:    "net0",
		IPConfig:     "ip=dhcp",
		Onboot:       false,
		AgentTimeout: defaultAgentTimeout,
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
		mcnflag.IntFlag{
			Name:   "pve-template-vmid",
			EnvVar: "PVE_TEMPLATE_VMID",
			Usage:  "Existing PVE VM template VMID used to clone new VMs from",
			Value:  0,
		},
		mcnflag.StringFlag{
			Name:   "pve-vmname",
			EnvVar: "PVE_VMNAME",
			Usage:  "Override the name assigned to the PVE VM. Defaults to the machine name",
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
			Name:   "pve-disk",
			EnvVar: "PVE_DISK",
			Usage:  "Disk size in GB (informational; template disk is used as-is)",
			Value:  20,
		},
		mcnflag.StringFlag{
			Name:   "pve-net-iface",
			EnvVar: "PVE_NET_IFACE",
			Usage:  "Name of the guest interface used to discover the machine IP",
		},
		mcnflag.StringFlag{
			Name:   "pve-net-device",
			EnvVar: "PVE_NET_DEVICE",
			Usage:  "PVE config device (net0..net31) whose MAC is used for IP discovery",
			Value:  "net0",
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
			Usage:  "Configure cloud-init (ipconfig0/sshkeys) on the cloned VM",
		},
		mcnflag.StringFlag{
			Name:   "pve-ipconfig",
			EnvVar: "PVE_IPCONFIG",
			Usage:  "cloud-init ipconfig0 string, e.g. ip=dhcp or ip=192.0.2.10/24,gw=192.0.2.1",
			Value:  "ip=dhcp",
		},
		mcnflag.StringFlag{
			Name:   "pve-sshkeys",
			EnvVar: "PVE_SSHKEYS",
			Usage:  "Cloud-init sshkeys string (URL or inline, newline-separated)",
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
		mcnflag.StringFlag{
			Name:   "ssh-user",
			EnvVar: "SSH_USER",
			Usage:  "SSH user used to log into the VM",
			Value:  defaultSSHUser,
		},
		mcnflag.IntFlag{
			Name:   "ssh-port",
			EnvVar: "SSH_PORT",
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
	d.TemplateVMID = flags.Int("pve-template-vmid")
	d.VMName = flags.String("pve-vmname")
	d.Cores = flags.Int("pve-cores")
	d.Sockets = flags.Int("pve-sockets")
	d.MemoryMB = flags.Int("pve-memory")
	d.DiskGB = flags.Int("pve-disk")
	d.NetIface = flags.String("pve-net-iface")
	d.NetDevice = flags.String("pve-net-device")
	if d.NetDevice == "" {
		d.NetDevice = "net0"
	}
	d.CloudInit = flags.Bool("pve-cloudinit")
	d.IPConfig = flags.String("pve-ipconfig")
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
	d.SSHUser = flags.String("ssh-user")
	d.SSHPort = flags.Int("ssh-port")
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

	vmName := d.VMName
	if vmName == "" {
		vmName = d.MachineName
	}

	assigned, err := d.client.CloneFromTemplate(ctx, d.TemplateVMID, d.VMID, vmName)
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

// finalizeCreate runs the post-clone steps (Configure + start + NIC MAC
// capture used by IP discovery). Broken out so the rollback in Create covers
// every failure mode with one cleanup path.
func (d *Driver) finalizeCreate(ctx context.Context, vmName string) error {
	onboot := d.Onboot
	opts := proxmox.VMOptions{
		Name:       vmName,
		Cores:      uint16(d.Cores),
		Sockets:    uint16(d.Sockets),
		Memory:     uint32(d.MemoryMB),
		Onboot:     &onboot,
		CloudInit:  d.CloudInit,
		IPConfig:   d.IPConfig,
		SSHKeys:    d.SSHKeys,
	}
	if err := d.client.Configure(ctx, d.VMID, opts); err != nil {
		return err
	}
	if err := d.client.Start(ctx, d.VMID); err != nil {
		return err
	}
	log.Infof("pve: starting VM %d", d.VMID)

	mac, err := d.client.VMNetMAC(ctx, d.VMID, d.NetDevice)
	if err != nil {
		log.Warnf("pve: could not read %s MAC for IP discovery: %v; falling back to first-IPv4 detection", d.NetDevice, err)
	} else {
		d.NetMAC = mac
		log.Infof("pve: VM %d uses MAC %s on %s", d.VMID, mac, d.NetDevice)
	}
	return nil
}

// Start powers on the VM if it is not already running.
func (d *Driver) Start() error {
	if err := d.init(); err != nil {
		return err
	}
	ctx := context.Background()
	st, err := d.client.State(ctx, d.VMID)
	if err != nil {
		return err
	}
	if st == state.Running.String() {
		return nil
	}
	return d.client.Start(ctx, d.VMID)
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
func (d *Driver) Remove() error {
	if err := d.init(); err != nil {
		return err
	}
	return d.client.Remove(context.Background(), d.VMID)
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
	switch st {
	case state.Running.String():
		return state.Running, nil
	case state.Paused.String():
		return state.Paused, nil
	case state.Stopped.String():
		return state.Stopped, nil
	}
	return state.Error, nil
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