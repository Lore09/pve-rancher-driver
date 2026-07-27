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