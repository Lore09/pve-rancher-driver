// Package proxmox wraps go-proxmox to provide a small surface for the
// docker-machine driver to interact with a Proxmox VE cluster using an
// API token for authentication.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	proxmox "github.com/luthermonson/go-proxmox"
)

// Client wraps the go-proxmox client with the small set of operations the
// docker-machine driver needs.
type Client struct {
	api  *proxmox.Client
	node string
}

// Config holds the connection parameters for a Proxmox VE endpoint.
type Config struct {
	// APIUrl is the base API URL, e.g. https://host:8006/api2/json
	APIUrl string
	// APITokenID is the Proxmox API token id, formatted as USER@REALM!TOKENID
	// (e.g. rancher@pve!machine).
	APITokenID string
	// APITokenSecret is the secret portion of the API token.
	APITokenSecret string
	// Node is the target PVE node name. If empty the first online node is used.
	Node string
	// Insecure disables TLS certificate verification.
	Insecure bool
	// CACertPEM is an optional PEM-encoded CA certificate used to verify the
	// PVE API endpoint. When non-empty it overrides the system roots. Only
	// honored when Insecure is false.
	CACertPEM string
	// Timeout is applied as the upper bound when waiting for PVE tasks.
	Timeout time.Duration
}

// New returns a configured Proxmox client.
func New(cfg Config) (*Client, error) {
	if cfg.APIUrl == "" {
		return nil, errors.New("proxmox: APIUrl is required")
	}
	if cfg.APITokenID == "" {
		return nil, errors.New("proxmox: API token id is required")
	}
	if cfg.APITokenSecret == "" {
		return nil, errors.New("proxmox: API token secret is required")
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.Insecure}
	if !cfg.Insecure && cfg.CACertPEM != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
			return nil, errors.New("proxmox: failed to parse provided CA certificate PEM")
		}
		tlsCfg.RootCAs = pool
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}

	api := proxmox.NewClient(cfg.APIUrl,
		proxmox.WithHTTPClient(httpClient),
		proxmox.WithAPIToken(cfg.APITokenID, cfg.APITokenSecret),
	)

	return &Client{api: api, node: cfg.Node}, nil
}

func (c *Client) waitTimeout() time.Duration {
	return 5 * time.Minute
}

// ResolveNode returns the node name to operate on. If the configured node is
// empty it picks the first online node in the cluster.
func (c *Client) ResolveNode(ctx context.Context) (string, error) {
	if c.node != "" {
		return c.node, nil
	}
	nodes, err := c.api.Nodes(ctx)
	if err != nil {
		return "", fmt.Errorf("proxmox: cannot list nodes: %w", err)
	}
	if len(nodes) == 0 {
		return "", errors.New("proxmox: no nodes available")
	}
	for _, n := range nodes {
		if n.Status == "online" {
			c.node = n.Node
			return c.node, nil
		}
	}
	c.node = nodes[0].Node
	return c.node, nil
}

func (c *Client) vm(ctx context.Context, vmid int) (*proxmox.VirtualMachine, error) {
	node, err := c.ResolveNode(ctx)
	if err != nil {
		return nil, err
	}
	n, err := c.api.Node(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("proxmox: node %q not reachable: %w", node, err)
	}
	return n.VirtualMachine(ctx, vmid)
}

// CloneFromTemplate clones the given template VMID into a new VM and returns
// the assigned VMID. If newVMID is 0 the cluster assigns the next free ID.
func (c *Client) CloneFromTemplate(ctx context.Context, templateVMID, newVMID int, name string) (int, error) {
	vm, err := c.vm(ctx, templateVMID)
	if err != nil {
		return 0, fmt.Errorf("proxmox: template %d not found: %w", templateVMID, err)
	}
	params := &proxmox.VirtualMachineCloneOptions{
		NewID: newVMID,
		Name:  name,
		Full:  1,
	}
	assigned, task, err := vm.Clone(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("proxmox: clone %d -> %d failed: %w", templateVMID, newVMID, err)
	}
	if task == nil {
		return assigned, nil
	}
	if err := task.Wait(ctx, time.Second, c.waitTimeout()); err != nil {
		return assigned, fmt.Errorf("proxmox: clone task did not complete: %w", err)
	}
	return assigned, nil
}

// Configure applies CPU, memory, network and disk overrides to a VM prior to
// starting it for the first time.
func (c *Client) Configure(ctx context.Context, vmid int, opts VMOptions) error {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return err
	}
	var options []proxmox.VirtualMachineOption
	if opts.Name != "" {
		options = append(options, proxmox.VirtualMachineOption{Name: "name", Value: opts.Name})
	}
	if opts.Cores > 0 {
		options = append(options, proxmox.VirtualMachineOption{Name: "cores", Value: opts.Cores})
	}
	if opts.Sockets > 0 {
		options = append(options, proxmox.VirtualMachineOption{Name: "sockets", Value: opts.Sockets})
	}
	if opts.Memory > 0 {
		options = append(options, proxmox.VirtualMachineOption{Name: "memory", Value: opts.Memory})
	}
	if opts.Onboot != nil {
		options = append(options, proxmox.VirtualMachineOption{Name: "onboot", Value: *opts.Onboot})
	}
	if opts.CloudInit {
		if opts.IPConfig != "" {
			options = append(options, proxmox.VirtualMachineOption{Name: "ipconfig0", Value: opts.IPConfig})
		}
		if opts.SSHKeys != "" {
			options = append(options, proxmox.VirtualMachineOption{Name: "sshkeys", Value: opts.SSHKeys})
		}
	}
	if len(options) == 0 {
		return nil
	}
	task, err := vm.Config(ctx, options...)
	if err != nil {
		return fmt.Errorf("proxmox: configure vm %d failed: %w", vmid, err)
	}
	if task != nil {
		if err := task.Wait(ctx, time.Second, c.waitTimeout()); err != nil {
			return fmt.Errorf("proxmox: configure task did not complete: %w", err)
		}
	}
	return nil
}

// Start powers on a VM, waiting for the start task to complete.
func (c *Client) Start(ctx context.Context, vmid int) error {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return err
	}
	task, err := vm.Start(ctx)
	if err != nil {
		return err
	}
	return c.waitTask(ctx, task)
}

// Stop gracefully shuts down a VM.
func (c *Client) Stop(ctx context.Context, vmid int) error {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return err
	}
	task, err := vm.Shutdown(ctx)
	if err != nil {
		return err
	}
	return c.waitTask(ctx, task)
}

// Kill forcefully powers off a VM (equivalent to physical power cut).
func (c *Client) Kill(ctx context.Context, vmid int) error {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return err
	}
	task, err := vm.Stop(ctx)
	if err != nil {
		return err
	}
	return c.waitTask(ctx, task)
}

// Restart reboots the guest VM (graceful).
func (c *Client) Restart(ctx context.Context, vmid int) error {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return err
	}
	task, err := vm.Reboot(ctx)
	if err != nil {
		return err
	}
	return c.waitTask(ctx, task)
}

// Remove permanently destroys a VM and its disks.
func (c *Client) Remove(ctx context.Context, vmid int) error {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return err
	}
	task, err := vm.Delete(ctx)
	if err != nil {
		return err
	}
	return c.waitTask(ctx, task)
}

// State returns the current running state of a VM ("running", "stopped", ...).
func (c *Client) State(ctx context.Context, vmid int) (string, error) {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return "", err
	}
	return vm.Status, nil
}

// GuestIP retrieves the first IPv4 address reported by the QEMU guest agent
// over the given interface. Requires the qemu-guest-agent to be installed
// and running inside the guest.
func (c *Client) GuestIP(ctx context.Context, vmid int, iface string) (string, error) {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return "", err
	}
	if err := vm.WaitForAgent(ctx, 60); err != nil {
		return "", fmt.Errorf("proxmox: guest agent not available: %w", err)
	}
	ifaces, err := vm.AgentGetNetworkIFaces(ctx)
	if err != nil {
		return "", fmt.Errorf("proxmox: guest agent network query failed: %w", err)
	}
	for _, n := range ifaces {
		if iface != "" && n.Name != iface {
			continue
		}
		for _, a := range n.IPAddresses {
			if a.IPAddress == "" || a.IPAddressType != "ipv4" {
				continue
			}
			if a.IPAddress == "127.0.0.1" {
				continue
			}
			return a.IPAddress, nil
		}
	}
	for _, n := range ifaces {
		for _, a := range n.IPAddresses {
			if a.IPAddress == "" || a.IPAddressType != "ipv4" || a.IPAddress == "127.0.0.1" {
				continue
			}
			return a.IPAddress, nil
		}
	}
	return "", errors.New("proxmox: no IPv4 address reported by guest agent")
}

func (c *Client) waitTask(ctx context.Context, task *proxmox.Task) error {
	if task == nil {
		return nil
	}
	return task.Wait(ctx, time.Second, c.waitTimeout())
}

// VMOptions is the subset of PVE VM attributes that the docker-machine driver
// allows overriding when cloning from a template.
type VMOptions struct {
	Name      string
	Cores     uint16
	Sockets   uint16
	Memory    uint32
	Onboot    *bool
	CloudInit bool
	IPConfig  string
	SSHKeys   string
}

// ---------- PVE version + permission probe ----------

// requiredPrivs returns the privileges the driver's API token must have in
// order to clone/config/start/read-guest-agent on a VM. The set differs
// between PVE major versions: 9.x introduced the more narrowly-scoped
// VM.GuestAgent.Audit in place of VM.Monitor for reading guest-agent data.
// Until PVE 8 reaches EOL (2026-08-31) we support both, picking based on the
// version reported by the live API endpoint.
var requiredPrivs = map[int][]string{
	9: {
		"VM.Clone", "VM.Allocate", "VM.Audit", "VM.PowerMgmt",
		"VM.Config.Disk", "VM.Config.CPU", "VM.Config.Memory",
		"VM.Config.Network", "VM.Config.Cloudinit", "VM.Config.Options",
		"VM.GuestAgent.Audit",
		"Datastore.AllocateSpace", "Datastore.Audit",
		"SDN.Use", "Pool.Allocate",
	},
	8: {
		"VM.Clone", "VM.Allocate", "VM.Audit", "VM.PowerMgmt",
		"VM.Config.Disk", "VM.Config.CPU", "VM.Config.Memory",
		"VM.Config.Network", "VM.Config.Cloudinit", "VM.Config.Options",
		"VM.Monitor",
		"Datastore.AllocateSpace", "Datastore.Audit",
		"SDN.Use", "Pool.Allocate",
	},
}

// Version returns the PVE server version as parsed major.minor and the raw
// release string. Used both for the permission-set dispatch and for any
// future version-gated behaviour.
func (c *Client) Version(ctx context.Context) (major int, raw string, err error) {
	v, err := c.api.Version(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("proxmox: cannot read server version: %w", err)
	}
	raw = v.Version
	major, _ = parsePVEMajor(v.Version)
	return major, raw, nil
}

// VerifyPermissions probes the API token's effective permissions and PVE major
// version together, returning a descriptive error if any required privilege is
// absent. The error lists every missing priv so the operator can paste the
// `pveum acl modify` line without a second round-trip.
//
// The probe exists because PVE API tokens with privilege separation (the
// default, recommended setting) start with zero privileges even if the parent
// user is an admin; the symptom of a missing ACL on the token itself is not an
// auth error but silent empty API responses — templated dropdowns come back
// blank, clones fail with vague messages. Failing fast in PreCreateCheck
// short-circuits that unhappy path.
func (c *Client) VerifyPermissions(ctx context.Context) error {
	major, _, err := c.Version(ctx)
	if err != nil {
		return err
	}
	needed, ok := requiredPrivs[major]
	if !ok {
		// Unknown future version — fall back to the most recent known set
		// (PVE 9). New privileges in 10.x will trigger missing-priv errors
		// here that an operator can resolve and then we'll update the map.
		needed = requiredPrivs[9]
	}

	perms, err := c.api.Permissions(ctx, nil)
	if err != nil {
		return fmt.Errorf("proxmox: cannot read token permissions: %w", err)
	}
	// Permissions is map[path]Permission where Permission is map[priv]IntOrBool.
	// We collapse every granted priv across every path into one set, since
	// every required priv is expected to be granted on "/" at minimum.
	granted := make(map[string]struct{}, 64)
	for _, p := range perms {
		for priv := range p {
			granted[priv] = struct{}{}
		}
	}
	var missing []string
	for _, priv := range needed {
		if _, ok := granted[priv]; !ok {
			missing = append(missing, priv)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"proxmox: API token is missing privileges required by the driver "+
				"(PVE %d): %s. Grant them to BOTH the user and the token, "+
				"see README \"Proxmox VE API token\" section",
			major, strings.Join(missing, ", "),
		)
	}
	return nil
}

// ---------- MAC-based IP detection ----------

// VMNetMAC retrieves the MAC address of the given net device on the named VM.
// device is a PVE config field name like "net0" (default if empty). The MAC is
// returned in lowercase colon-separated form (e.g. "aa:bb:cc:dd:ee:ff"); an
// empty string is returned if the VM has no such device or the entry omits the
// MAC (PVE itself may generate one on first boot — callers should re-read
// shortly after clone to give the cluster time to assign it).
func (c *Client) VMNetMAC(ctx context.Context, vmid int, device string) (string, error) {
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return "", err
	}
	if vm.VirtualMachineConfig == nil {
		return "", errors.New("proxmox: VM config not loaded")
	}
	if device == "" {
		device = "net0"
	}
	raw := netDeviceFromConfig(vm.VirtualMachineConfig, device)
	if raw == "" {
		return "", fmt.Errorf("proxmox: vm %d has no %q device configured", vmid, device)
	}
	mac := parseNetMAC(raw)
	if mac == "" {
		return "", fmt.Errorf("proxmox: %s entry %q has no MAC; PVE may not have assigned one yet", device, raw)
	}
	return mac, nil
}

// netDeviceFromConfig pulls the named net<N> field off the VM config struct.
// VirtualMachineConfig exposes Net0..Net31 as plain string fields, so we
// fall through to a small switch rather than reflect on every call.
func netDeviceFromConfig(cfg *proxmox.VirtualMachineConfig, device string) string {
	switch device {
	case "net0":
		return cfg.Net0
	case "net1":
		return cfg.Net1
	case "net2":
		return cfg.Net2
	case "net3":
		return cfg.Net3
	case "net4":
		return cfg.Net4
	case "net5":
		return cfg.Net5
	case "net6":
		return cfg.Net6
	case "net7":
		return cfg.Net7
	case "net8":
		return cfg.Net8
	case "net9":
		return cfg.Net9
	}
	return ""
}

// parseNetMAC extracts the MAC from a PVE net<N> config value. The value is a
// comma-separated list whose first item is "model=MAC" (e.g.
// "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1"). The MAC field is
// optional in the API — PVE generates one on demand — so we return "" when no
// MAC is present rather than erroring.
func parseNetMAC(raw string) string {
	first := raw
	if i := strings.Index(raw, ","); i >= 0 {
		first = raw[:i]
	}
	if i := strings.Index(first, "="); i >= 0 {
		first = first[i+1:]
	}
	macPattern := regexp.MustCompile(`(?i)^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	if macPattern.MatchString(first) {
		return strings.ToLower(first)
	}
	return ""
}

// GuestIPByMAC waits for the QEMU guest agent and returns the first IPv4
// address reported on the interface whose hardware-address matches the given
// MAC. This avoids the common failure mode where the guest agent reports IPs
// on Docker/CNI bridges or IPv6 link-local addresses before DHCP has finished
// on the actual NIC we cloned for. mac may be empty, in which case GuestIP
// (the older, less-strict helper) is the right fallback.
func (c *Client) GuestIPByMAC(ctx context.Context, vmid int, mac string) (string, error) {
	if mac == "" {
		return c.GuestIP(ctx, vmid, "")
	}
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return "", err
	}
	if err := vm.WaitForAgent(ctx, 60); err != nil {
		return "", fmt.Errorf("proxmox: guest agent not available: %w", err)
	}
	ifaces, err := vm.AgentGetNetworkIFaces(ctx)
	if err != nil {
		return "", fmt.Errorf("proxmox: guest agent network query failed: %w", err)
	}
	lower := strings.ToLower(mac)
	for _, n := range ifaces {
		if strings.ToLower(n.HardwareAddress) != lower {
			continue
		}
		for _, a := range n.IPAddresses {
			if a.IPAddress == "" || a.IPAddressType != "ipv4" {
				continue
			}
			if a.IPAddress == "127.0.0.1" {
				continue
			}
			return a.IPAddress, nil
		}
	}
	return "", fmt.Errorf("proxmox: no IPv4 address reported by guest agent for interface with MAC %s", mac)
}

// parsePVEMajor pulls the major version out of a PVE release string like
// "8.2.4" or "9.0/no-subscription". Returns 0 if the format is unrecognized.
func parsePVEMajor(s string) (int, error) {
	if i := strings.IndexAny(s, "/ "); i > 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 {
		return 0, errors.New("proxmox: empty version string")
	}
	return strconv.Atoi(parts[0])
}