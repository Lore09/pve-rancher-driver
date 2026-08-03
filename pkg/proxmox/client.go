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
	"sort"
	"strconv"
	"strings"
	"time"

	proxmox "github.com/luthermonson/go-proxmox"
)

// Client wraps the go-proxmox client with the small set of operations the
// docker-machine driver needs.
type Client struct {
	api          *proxmox.Client
	node         string
	allowedNodes []string
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
	// Node is the target PVE node name. If empty, ResolveNode picks
	// automatically from AllowedNodes (or every node, if that is also empty).
	Node string
	// AllowedNodes restricts automatic node selection to this set. Ignored
	// when Node is set. Empty means every online node in the cluster is a
	// candidate.
	AllowedNodes []string
	// Insecure disables TLS certificate verification.
	Insecure bool
	// CACertPEM is an optional PEM-encoded CA certificate used to verify the
	// PVE API endpoint. When non-empty it overrides the system roots. Only
	// honored when Insecure is false.
	CACertPEM string
	// Timeout bounds every individual HTTP request to the PVE API. Zero uses
	// defaultRequestTimeout.
	//
	// Without this, a PVE host that stops responding mid-request (network
	// partition, host down, reboot) hangs the call forever: net/http's
	// default client has no timeout, and neither task.Wait's polling loop
	// nor the driver's own operations set a context deadline, so nothing
	// ever interrupts the stuck request. That single hung call is enough to
	// stall a whole cluster teardown, since Rancher removes machines in a
	// cluster one at a time and never reaches the ones after it.
	Timeout time.Duration
}

// defaultRequestTimeout is used when Config.Timeout is zero. It bounds a
// single PVE API request, not a whole operation — task.Wait already polls in
// a loop capped by waitTimeout, and WaitForAgent similarly polls rather than
// holding one request open, so 30s is generous for any one call without
// cutting either of those loops short.
const defaultRequestTimeout = 30 * time.Second

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

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}

	api := proxmox.NewClient(cfg.APIUrl,
		proxmox.WithHTTPClient(httpClient),
		proxmox.WithAPIToken(cfg.APITokenID, cfg.APITokenSecret),
	)

	return &Client{api: api, node: cfg.Node, allowedNodes: cfg.AllowedNodes}, nil
}

func (c *Client) waitTimeout() time.Duration {
	return 5 * time.Minute
}

// ResolveNode returns the node this client operates against, resolving and
// caching it on first use. If the configured node is empty it picks
// automatically: the online node (restricted to AllowedNodes, if set) with
// the most free memory. On a single-node install there is only ever one
// candidate, so this is equivalent to the old "first online node" behaviour.
//
// The result is cached in c.node for the lifetime of the Client, so every
// subsequent operation — including ones made by a later, separate driver
// invocation that was configured with the node this resolved to — targets
// the same node a VM was actually created on.
func (c *Client) ResolveNode(ctx context.Context) (string, error) {
	if c.node != "" {
		return c.node, nil
	}
	nodes, err := c.api.Nodes(ctx)
	if err != nil {
		return "", fmt.Errorf("proxmox: cannot list nodes: %w", err)
	}
	chosen, err := selectNode(nodes, c.allowedNodes)
	if err != nil {
		return "", err
	}
	c.node = chosen
	return c.node, nil
}

// CurrentNode returns the node ResolveNode has already resolved to, or "" if
// it has not been called yet.
func (c *Client) CurrentNode() string {
	return c.node
}

// selectNode picks the best candidate node for a new VM: the online node
// with the most free memory, restricted to allowed when it is non-empty.
// Ties break on node name so the choice is deterministic.
//
// Proxmox does not forbid overcommitting a node's memory, so this never
// hard-fails on a shortfall — it always returns the least-loaded candidate
// and lets Proxmox itself accept or reject the clone.
func selectNode(nodes proxmox.NodeStatuses, allowed []string) (string, error) {
	allowSet := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		allowSet[n] = true
	}

	var candidates proxmox.NodeStatuses
	for _, n := range nodes {
		if !strings.EqualFold(n.Status, "online") {
			continue
		}
		if len(allowSet) > 0 && !allowSet[n.Node] {
			continue
		}
		candidates = append(candidates, n)
	}
	if len(candidates) == 0 {
		if len(allowSet) > 0 {
			return "", fmt.Errorf("proxmox: none of --pve-allowed-nodes (%s) are online", strings.Join(allowed, ", "))
		}
		return "", errors.New("proxmox: no online PVE node available")
	}

	sort.Slice(candidates, func(i, j int) bool {
		fi, fj := freeMem(candidates[i]), freeMem(candidates[j])
		if fi != fj {
			return fi > fj
		}
		return candidates[i].Node < candidates[j].Node
	})
	return candidates[0].Node, nil
}

// freeMem returns a node's unallocated memory in bytes, floored at 0 — PVE
// can legitimately report Mem slightly above MaxMem transiently (e.g. right
// after a balloon adjustment), and a negative "free" value would otherwise
// make selectNode rank an overcommitted node as having headroom.
func freeMem(n *proxmox.NodeStatus) int64 {
	free := int64(n.MaxMem) - int64(n.Mem)
	if free < 0 {
		return 0
	}
	return free
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

// clusterResources returns every resource (VMs, containers, storages,
// nodes, ...) the cluster currently knows about, unfiltered. Callers filter
// by Type/VMID/Node themselves.
func (c *Client) clusterResources(ctx context.Context) ([]proxmox.ClusterResource, error) {
	var resources []proxmox.ClusterResource
	if err := c.api.Get(ctx, "/cluster/resources", &resources); err != nil {
		return nil, fmt.Errorf("proxmox: cannot list cluster resources: %w", err)
	}
	return resources, nil
}

// nodeOf returns the node currently hosting vmid, discovered from cluster
// resources rather than assumed from ResolveNode: a template's disk lives
// wherever it was created, which may not be the node ResolveNode picks as
// the *destination* for a new VM once node scheduling is in play.
func (c *Client) nodeOf(ctx context.Context, vmid int) (string, error) {
	resources, err := c.clusterResources(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range resources {
		if int(r.VMID) == vmid && r.Node != "" {
			return r.Node, nil
		}
	}
	return "", fmt.Errorf("proxmox: VMID %d not found anywhere in the cluster", vmid)
}

// CloneOptions configures a single template clone.
type CloneOptions struct {
	// NewID is the VMID to assign the clone. 0 lets Proxmox assign the next
	// free id.
	NewID int
	// Name is the new VM's name.
	Name string
	// Linked selects a linked clone (a thin overlay referencing the
	// template's disk) instead of a full clone (a complete byte-for-byte
	// copy). A linked clone is created almost instantly regardless of the
	// template's size, which matters when several machines in a pool clone
	// at once and would otherwise contend for the same storage's I/O — but
	// every linked clone stays dependent on the template disk for as long
	// as it exists, and not every storage backend supports it.
	Linked bool
	// Pool, if set, places the new VM into this PVE resource pool as part
	// of the clone call itself — atomically, so there is no window after
	// creation where the VM exists but is not yet a pool member. This is
	// what makes a pool-scoped API token ACL (rather than one granted on
	// `/`) actually protect every other VM in the cluster: the token can
	// only ever act on VMs inside the pool, and every VM this driver
	// creates lands there from the moment it exists.
	Pool string
}

// CloneFromTemplate clones the given template VMID into a new VM according
// to opts and returns the assigned VMID.
//
// The clone's source node is wherever the template actually lives, which is
// looked up independently of ResolveNode's destination: with node scheduling
// in play the two can differ, in which case the destination is passed to PVE
// as the clone's target node. PVE only allows that cross-node when the
// template's disk is on shared storage — on local storage the clone fails
// with a clear PVE-side error, which is the right outcome (there is no
// client-side way to know in advance whether a given storage is shared).
func (c *Client) CloneFromTemplate(ctx context.Context, templateVMID int, opts CloneOptions) (int, error) {
	templateNode, err := c.nodeOf(ctx, templateVMID)
	if err != nil {
		return 0, fmt.Errorf("proxmox: template %d not found: %w", templateVMID, err)
	}
	n, err := c.api.Node(ctx, templateNode)
	if err != nil {
		return 0, fmt.Errorf("proxmox: node %q not reachable: %w", templateNode, err)
	}
	vm, err := n.VirtualMachine(ctx, templateVMID)
	if err != nil {
		return 0, fmt.Errorf("proxmox: template %d not found: %w", templateVMID, err)
	}

	target, err := c.ResolveNode(ctx)
	if err != nil {
		return 0, err
	}

	full := uint8(1)
	if opts.Linked {
		full = 0
	}
	params := &proxmox.VirtualMachineCloneOptions{
		NewID: opts.NewID,
		Name:  opts.Name,
		Full:  full,
		Pool:  opts.Pool,
	}
	if target != templateNode {
		params.Target = target
	}
	assigned, task, err := vm.Clone(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("proxmox: clone %d -> %d failed: %w", templateVMID, opts.NewID, err)
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
	// Enable the PVE-side guest agent channel unconditionally. Every IP
	// discovery path in this driver goes through the guest agent, and a
	// template built without `--agent 1` is the single most common reason a
	// node never reports an address. Forcing it here means the template only
	// has to have qemu-guest-agent installed, not correctly configured.
	options = append(options, proxmox.VirtualMachineOption{Name: "agent", Value: "1"})
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
	if opts.Tags != "" {
		options = append(options, proxmox.VirtualMachineOption{Name: "tags", Value: opts.Tags})
	}
	if opts.Description != "" {
		options = append(options, proxmox.VirtualMachineOption{Name: "description", Value: opts.Description})
	}
	if opts.CloudInit {
		if opts.IPConfig != "" {
			options = append(options, proxmox.VirtualMachineOption{Name: "ipconfig0", Value: opts.IPConfig})
		}
		if opts.Nameserver != "" {
			options = append(options, proxmox.VirtualMachineOption{Name: "nameserver", Value: opts.Nameserver})
		}
		if opts.SearchDomain != "" {
			options = append(options, proxmox.VirtualMachineOption{Name: "searchdomain", Value: opts.SearchDomain})
		}
		if opts.CIUser != "" {
			options = append(options, proxmox.VirtualMachineOption{Name: "ciuser", Value: opts.CIUser})
		}
		if opts.SSHKeys != "" {
			options = append(options, proxmox.VirtualMachineOption{Name: "sshkeys", Value: opts.SSHKeys})
		}
	}
	// Rewriting net<N> without an explicit macaddr= makes PVE generate a fresh
	// MAC for the device. That is safe here only because the driver captures
	// the MAC (VMNetMAC) *after* Configure returns — do not move that capture
	// earlier, or MAC-matched IP discovery will match a stale address.
	if opts.NetBridge != "" {
		options = append(options, proxmox.VirtualMachineOption{
			Name:  netDeviceKey(opts.NetDevice),
			Value: buildNetValue(opts),
		})
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

// netDeviceKey normalises a PVE net device config key, defaulting to net0.
func netDeviceKey(device string) string {
	if device == "" {
		return "net0"
	}
	return device
}

// buildNetValue renders a PVE net<N> config value from the network fields of
// opts, e.g. "model=virtio,bridge=vmbr1,tag=42,mtu=9000,firewall=1". Only the
// fields the caller actually set are emitted so PVE applies its own defaults
// for the rest.
func buildNetValue(opts VMOptions) string {
	model := opts.NetModel
	if model == "" {
		model = "virtio"
	}
	v := fmt.Sprintf("model=%s,bridge=%s", model, opts.NetBridge)
	if opts.NetVlanTag > 0 {
		v += fmt.Sprintf(",tag=%d", opts.NetVlanTag)
	}
	if opts.NetMTU > 0 {
		v += fmt.Sprintf(",mtu=%d", opts.NetMTU)
	}
	if opts.NetFirewall != nil {
		firewall := 0
		if *opts.NetFirewall {
			firewall = 1
		}
		v += fmt.Sprintf(",firewall=%d", firewall)
	}
	return v
}

// ResizeBootDisk grows the given disk (e.g. "scsi0") of a VM to sizeGB.
//
// PVE can only ever *grow* a disk: passing a size smaller than the template's
// disk is rejected by the API, so callers should skip the call rather than
// pass a smaller value. go-proxmox has no VM resize helper (only the LXC
// equivalent), hence the raw PUT plus manual task wait.
func (c *Client) ResizeBootDisk(ctx context.Context, vmid int, disk string, sizeGB int) error {
	if disk == "" {
		return errors.New("proxmox: disk device is required to resize")
	}
	if sizeGB <= 0 {
		return errors.New("proxmox: disk size must be greater than 0")
	}
	node, err := c.ResolveNode(ctx)
	if err != nil {
		return err
	}
	var upid proxmox.UPID
	body := map[string]interface{}{
		"disk": disk,
		"size": fmt.Sprintf("%dG", sizeGB),
	}
	if err := c.api.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), body, &upid); err != nil {
		return fmt.Errorf("proxmox: resize %s of vm %d to %dGB failed: %w", disk, vmid, sizeGB, err)
	}
	// A resize on some storage backends completes synchronously and returns an
	// empty UPID; only wait when PVE actually handed back a task.
	if upid == "" {
		return nil
	}
	if err := c.waitTask(ctx, proxmox.NewTask(upid, c.api)); err != nil {
		return fmt.Errorf("proxmox: resize task for vm %d did not complete: %w", vmid, err)
	}
	return nil
}

// buildDiskValue renders the PVE config value for one data disk. The key order
// is fixed so the value is stable and diffable in tests and in PVE's UI.
//
// serial= is the load-bearing part: it is what the guest sees at
// /dev/disk/by-id and in `lsblk -o NAME,SERIAL`, which is how the driver finds
// the disk again without depending on sd* ordering.
func buildDiskValue(spec DiskSpec) string {
	return fmt.Sprintf("%s:%d,serial=%s,discard=%s,iothread=%s,backup=%s",
		spec.Storage, spec.Size, spec.Label, spec.Discard, spec.IOThread, spec.Backup)
}

// allocateDiskSlots decides which scsi<N> key each spec gets. Specs naming a
// device keep it; the rest are filled from the lowest free slot upwards.
// scsi0 is never allocated — that is the boot disk.
func allocateDiskSlots(cfg map[string]interface{}, specs []DiskSpec) ([]string, error) {
	used := make(map[string]bool, len(cfg))
	for key := range cfg {
		used[key] = true
	}
	slots := make([]string, len(specs))

	// Explicit requests are claimed first so automatic allocation cannot steal
	// a slot a later spec asked for by name.
	for i, spec := range specs {
		if spec.Device == "" {
			continue
		}
		if used[spec.Device] {
			return nil, fmt.Errorf("proxmox: disk slot %s is already in use", spec.Device)
		}
		used[spec.Device] = true
		slots[i] = spec.Device
	}

	next := 1
	for i := range specs {
		if slots[i] != "" {
			continue
		}
		for ; next <= 30; next++ {
			name := fmt.Sprintf("scsi%d", next)
			if !used[name] {
				used[name] = true
				slots[i] = name
				break
			}
		}
		if slots[i] == "" {
			return nil, errors.New("proxmox: no free SCSI slot left for data disks")
		}
	}
	return slots, nil
}

// AddDisks attaches every data disk in one config update and reports which slot
// each one landed on. Doing it in a single PUT means one PVE task to wait for
// and no window where a VM carries half its disks.
//
// Call it before starting the VM so the guest sees all disks at boot.
func (c *Client) AddDisks(ctx context.Context, vmid int, specs []DiskSpec) ([]AttachedDisk, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	for _, spec := range specs {
		if spec.Storage == "" {
			return nil, errors.New("proxmox: storage is required to add a disk")
		}
		if spec.Size <= 0 {
			return nil, errors.New("proxmox: disk size must be greater than 0")
		}
		if spec.Label == "" {
			return nil, errors.New("proxmox: disk label is required; it becomes the disk serial")
		}
	}
	node, err := c.ResolveNode(ctx)
	if err != nil {
		return nil, err
	}
	// Read the raw config map so we can scan scsi<N> keys without enumerating
	// the 31 individual struct fields on VirtualMachineConfig.
	raw := map[string]interface{}{}
	if err := c.api.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), &raw); err != nil {
		return nil, fmt.Errorf("proxmox: cannot read vm %d config: %w", vmid, err)
	}
	slots, err := allocateDiskSlots(raw, specs)
	if err != nil {
		return nil, err
	}
	vm, err := c.vm(ctx, vmid)
	if err != nil {
		return nil, err
	}
	opts := make([]proxmox.VirtualMachineOption, 0, len(specs))
	attached := make([]AttachedDisk, 0, len(specs))
	for i, spec := range specs {
		opts = append(opts, proxmox.VirtualMachineOption{Name: slots[i], Value: buildDiskValue(spec)})
		attached = append(attached, AttachedDisk{Device: slots[i], Spec: spec})
	}
	task, err := vm.Config(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("proxmox: attaching %d data disk(s) to vm %d failed: %w", len(specs), vmid, err)
	}
	if err := c.waitTask(ctx, task); err != nil {
		return nil, fmt.Errorf("proxmox: data disk task for vm %d did not complete: %w", vmid, err)
	}
	return attached, nil
}

// UsedVMIDs returns every VMID currently present anywhere in the cluster.
//
// The query is deliberately unfiltered: Proxmox shares one ID space between
// QEMU VMs and LXC containers, and templates occupy IDs too, so filtering to
// `type=vm` would happily hand back an ID an existing container is using.
// Entries with no VMID (nodes, storages) report 0 and are skipped.
func (c *Client) UsedVMIDs(ctx context.Context) (map[int]bool, error) {
	resources, err := c.clusterResources(ctx)
	if err != nil {
		return nil, err
	}
	used := make(map[int]bool, len(resources))
	for _, r := range resources {
		if r.VMID > 0 {
			used[int(r.VMID)] = true
		}
	}
	return used, nil
}

// NextFreeVMID returns the lowest unused VMID within [minID, maxID].
//
// This is inherently advisory: the ID is free when this returns and may be
// taken by the time the caller clones into it, so callers creating several
// machines at once must be prepared to retry.
func (c *Client) NextFreeVMID(ctx context.Context, minID, maxID int) (int, error) {
	used, err := c.UsedVMIDs(ctx)
	if err != nil {
		return 0, err
	}
	for id := minID; id <= maxID; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("proxmox: no free VMID in range %d-%d — every id in it is already in use", minID, maxID)
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
		// Deleting something already gone is a success, not a failure:
		// otherwise a VM removed by hand leaves Rancher retrying its machine
		// deletion forever.
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	task, err := vm.Delete(ctx)
	if err != nil {
		return err
	}
	return c.waitTask(ctx, task)
}

// IsNotFound reports whether an error means the VM does not exist.
//
// PVE has no machine-readable "not found" for this: a missing VM surfaces as
// a 500 whose message names the absent config file. Matching on that text is
// fragile, so it is used only to turn a delete into a no-op — never to
// suppress an error that might mean something else.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such vm") ||
		strings.Contains(msg, "not found")
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
	Name    string
	Cores   uint16
	Sockets uint16
	Memory  uint32
	Onboot  *bool
	// Tags is PVE's semicolon-separated `tags` config value. Purely
	// informational to PVE itself, but it is how a VM created by this driver
	// is identified/filtered in the PVE UI without opening it.
	Tags string
	// Description is PVE's free-text `description` config value, rendered as
	// the Notes panel in the PVE UI. A clone inherits the template's notes,
	// which describe the template rather than the machine, so the driver
	// always overwrites it with something that identifies who created this VM
	// and what it belongs to.
	Description  string
	CloudInit    bool
	IPConfig     string
	Nameserver   string
	SearchDomain string
	CIUser       string
	// SSHKeys must already be URL-encoded as PVE expects for the sshkeys
	// config value (one OpenSSH key per line, percent-encoded).
	SSHKeys string

	// NetDevice is the PVE config key the network settings below are written
	// to ("net0" when empty). Nothing is written unless NetBridge is set.
	NetDevice string
	// NetBridge is the PVE bridge to attach the NIC to, e.g. "vmbr1". This is
	// the switch for the whole net<N> rewrite: when empty the template's own
	// network configuration is left completely untouched.
	NetBridge string
	// NetModel is the emulated NIC model ("virtio" when empty).
	NetModel string
	// NetVlanTag is the 802.1Q VLAN tag; 0 leaves the NIC untagged.
	NetVlanTag int
	// NetMTU overrides the NIC MTU; 0 uses the PVE default.
	NetMTU int
	// NetFirewall toggles the PVE firewall on the NIC; nil leaves it at the
	// PVE default.
	NetFirewall *bool
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
		// Sys.Audit is needed for GET /nodes/{node}/status, which the client
		// calls before touching any VM. Without it every operation fails with
		// "not authorized to access endpoint" *after* PreCreateCheck has
		// passed, i.e. with a half-created machine, so it belongs here.
		"Sys.Audit",
	},
	8: {
		"VM.Clone", "VM.Allocate", "VM.Audit", "VM.PowerMgmt",
		"VM.Config.Disk", "VM.Config.CPU", "VM.Config.Memory",
		"VM.Config.Network", "VM.Config.Cloudinit", "VM.Config.Options",
		"VM.Monitor",
		"Datastore.AllocateSpace", "Datastore.Audit",
		"SDN.Use", "Pool.Allocate",
		// See the PVE 9 set above: required for /nodes/{node}/status.
		"Sys.Audit",
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
