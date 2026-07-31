package driver

import (
	"strings"
	"testing"
)

func TestParseIPMode(t *testing.T) {
	tests := []struct {
		in      string
		want    ipMode
		wantErr bool
	}{
		{in: "", want: ipModeDHCP},
		{in: "dhcp", want: ipModeDHCP},
		{in: "DHCP", want: ipModeDHCP},
		{in: "  static  ", want: ipModeStatic},
		{in: "Static", want: ipModeStatic},
		{in: "manual", wantErr: true},
		{in: "none", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseIPMode(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseIPMode(%q) = %q, want an error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseIPMode(%q) returned error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseIPMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// PVE stores the nameserver string without validating it, so a typo becomes a
// node whose DNS silently does not work. Reject it here instead.
func TestNormalizeNameservers(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "empty means inherit from DHCP", in: "", want: ""},
		{name: "whitespace only means inherit", in: "   ", want: ""},
		{name: "single address", in: "1.1.1.1", want: "1.1.1.1"},
		{name: "space separated is passed through", in: "1.1.1.1 8.8.8.8", want: "1.1.1.1 8.8.8.8"},
		{name: "comma separated is normalized to spaces", in: "1.1.1.1,8.8.8.8", want: "1.1.1.1 8.8.8.8"},
		{name: "comma plus space does not produce empty entries", in: "1.1.1.1, 8.8.8.8", want: "1.1.1.1 8.8.8.8"},
		{name: "ipv6 resolvers are accepted", in: "2606:4700:4700::1111", want: "2606:4700:4700::1111"},
		{name: "a hostname is rejected", in: "dns.example.com", wantErr: "dns.example.com"},
		{name: "a malformed address is rejected", in: "1.1.1.999", wantErr: "1.1.1.999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeNameservers(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("normalizeNameservers(%q) = %q, want an error mentioning %q", tt.in, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeNameservers(%q) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("normalizeNameservers(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildIPConfigDHCP(t *testing.T) {
	// In dhcp mode the pool fields are irrelevant and must be ignored, not
	// validated: a pool that switches static -> dhcp may still carry them.
	got, err := buildIPConfig(ipModeDHCP, "192.168.15.150", "192.168.15.159", "24", "192.168.15.1", 250, 200)
	if err != nil {
		t.Fatalf("buildIPConfig() returned error: %v", err)
	}
	if got != "ip=dhcp" {
		t.Errorf("buildIPConfig() = %q, want %q", got, "ip=dhcp")
	}

	got, err = buildIPConfig(ipModeDHCP, "", "", "", "", 0, 0)
	if err != nil {
		t.Fatalf("buildIPConfig() with empty pool fields returned error: %v", err)
	}
	if got != "ip=dhcp" {
		t.Errorf("buildIPConfig() = %q, want %q", got, "ip=dhcp")
	}
}

func TestBuildIPConfigStatic(t *testing.T) {
	tests := []struct {
		name                   string
		start, end, prefix, gw string
		vmid, vmidMin          int
		want                   string
		wantErr                string
	}{
		{
			name:  "the first VMID in the range gets the start address",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "24", gw: "192.168.15.1",
			vmid: 200, vmidMin: 200,
			want: "ip=192.168.15.150/24,gw=192.168.15.1",
		},
		{
			name:  "the offset is added to the start address",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "24", gw: "192.168.15.1",
			vmid: 202, vmidMin: 200,
			want: "ip=192.168.15.152/24,gw=192.168.15.1",
		},
		{
			name:  "the last address in the pool is usable",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "24", gw: "192.168.15.1",
			vmid: 209, vmidMin: 200,
			want: "ip=192.168.15.159/24,gw=192.168.15.1",
		},
		{
			// The pool, not the netmask, caps the machine count.
			name:  "one machine past the end of the pool is exhaustion",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "24", gw: "192.168.15.1",
			vmid: 210, vmidMin: 200,
			wantErr: "static IP pool exhausted: 192.168.15.150-192.168.15.159 holds 10 machines",
		},
		{
			// The whole point of a separate prefix: a pool far from the gateway
			// still gets the real network's netmask.
			name:  "the offset carries across an octet boundary on a /16",
			start: "10.10.20.250", end: "10.10.21.10", prefix: "16", gw: "10.10.0.1",
			vmid: 210, vmidMin: 200,
			want: "ip=10.10.21.4/16,gw=10.10.0.1",
		},
		{
			name:  "a VMID below the range minimum is rejected",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "24", gw: "192.168.15.1",
			vmid: 199, vmidMin: 200,
			wantErr: "below",
		},
		{
			name:  "an end below the start is rejected",
			start: "192.168.15.159", end: "192.168.15.150", prefix: "24", gw: "192.168.15.1",
			vmid: 200, vmidMin: 200,
			wantErr: "--pve-ip-end 192.168.15.150 is below --pve-ip-start 192.168.15.159",
		},
		{
			// A prefix too narrow to hold both ends would give the machines a
			// netmask that does not describe the network they are on.
			name:  "a pool straddling two subnets is rejected",
			start: "192.168.15.150", end: "192.168.15.170", prefix: "28", gw: "192.168.15.1",
			vmid: 200, vmidMin: 200,
			wantErr: "not in the same /28 subnet",
		},
		{
			name:  "a pool containing the network address is rejected",
			start: "192.168.15.0", end: "192.168.15.10", prefix: "24", gw: "192.168.15.1",
			vmid: 200, vmidMin: 200,
			wantErr: "network address",
		},
		{
			name:  "a pool containing the broadcast address is rejected",
			start: "192.168.15.250", end: "192.168.15.255", prefix: "24", gw: "192.168.15.1",
			vmid: 200, vmidMin: 200,
			wantErr: "broadcast address",
		},
		{
			name:  "an IPv6 start is rejected",
			start: "2001:db8::10", end: "2001:db8::20", prefix: "24", gw: "2001:db8::1",
			vmid: 200, vmidMin: 200,
			wantErr: "IPv6",
		},
		{
			name:  "a non-numeric prefix is rejected",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "/24bits", gw: "192.168.15.1",
			vmid: 200, vmidMin: 200,
			wantErr: "--pve-ip-prefix",
		},
		{
			name:  "a prefix with no usable hosts is rejected",
			start: "192.168.15.150", end: "192.168.15.150", prefix: "31", gw: "192.168.15.1",
			vmid: 200, vmidMin: 200,
			wantErr: "out of range",
		},
		{
			// Written with a leading slash, as the UI placeholder shows it.
			name:  "a prefix written as /24 is accepted",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "/24", gw: "192.168.15.1",
			vmid: 200, vmidMin: 200,
			want: "ip=192.168.15.150/24,gw=192.168.15.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildIPConfig(ipModeStatic, tt.start, tt.end, tt.prefix, tt.gw, tt.vmid, tt.vmidMin)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("buildIPConfig() = %q, want an error mentioning %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildIPConfig() returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("buildIPConfig() = %q, want %q", got, tt.want)
			}
		})
	}
}

// validateStaticPool is a save-time check on the pool and gateway. It
// deliberately does NOT require the pool to be as large as the VMID range:
// NextFreeVMID hands out the lowest free id, so machines fill the pool from its
// start upward. Capacity is enforced per machine by buildIPConfig.
func TestValidateStaticPool(t *testing.T) {
	tests := []struct {
		name                   string
		start, end, prefix, gw string
		lo, hi                 int
		wantErr                string
	}{
		{
			name:  "a pool smaller than the VMID range is accepted",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "24", gw: "192.168.15.1",
			lo: 200, hi: 299,
		},
		{
			// The case that started this: the gateway is outside the pool, which
			// is normal, but inside the subnet, which is what matters.
			name:  "a gateway outside the pool but inside the subnet is accepted",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "24", gw: "192.168.15.1",
			lo: 200, hi: 209,
		},
		{
			// The /28 mistake: it narrows the subnet to .144-.159 so .1 is no
			// longer on-link, and the node would boot with no default route.
			// The pool stops at .158 here so the broadcast check does not fire
			// first and mask the gateway error being tested.
			name:  "a gateway outside the subnet is rejected",
			start: "192.168.15.150", end: "192.168.15.158", prefix: "28", gw: "192.168.15.1",
			lo: 200, hi: 299,
			wantErr: "outside 192.168.15.144/28",
		},
		{
			// The exact config that prompted this design: /28 also drags .159
			// into being the broadcast address, which is caught first.
			name:  "a /28 pool ending on the subnet broadcast is rejected",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "28", gw: "192.168.15.1",
			lo: 200, hi: 299,
			wantErr: "broadcast address of 192.168.15.144/28",
		},
		{
			name:  "a malformed gateway is rejected",
			start: "192.168.15.150", end: "192.168.15.159", prefix: "24", gw: "not-an-ip",
			lo: 200, hi: 299,
			wantErr: "--pve-gateway",
		},
		{
			name:  "a single-address pool is accepted",
			start: "192.168.15.150", end: "192.168.15.150", prefix: "24", gw: "192.168.15.1",
			lo: 200, hi: 299,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStaticPool(tt.start, tt.end, tt.prefix, tt.gw, tt.lo, tt.hi)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("validateStaticPool() = nil, want an error mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateStaticPool() returned error: %v", err)
			}
		})
	}
}

// newValidatedDriver returns a Driver with the minimum config PreCreateCheck
// needs before it reaches the addressing checks. SkipPermCheck avoids any
// network call, so these tests stay hermetic.
func newValidatedDriver() *Driver {
	d := NewDriver("test", "/tmp").(*Driver)
	d.APIUrl = "https://pve.example.com:8006/api2/json"
	d.APITokenID = "rancher@pve!machine"
	d.APITokenSecret = "secret"
	d.TemplateVMID = 9000
	d.SkipPermCheck = true

	return d
}

func TestPreCreateCheckStaticRequiresVMIDRange(t *testing.T) {
	d := newValidatedDriver()
	d.CloudInit = true // validateAddressing checks cloud-init before the range
	d.IPMode = "static"
	d.IPStart = "10.10.20.10"
	d.IPEnd = "10.10.20.109"
	d.IPPrefix = "24"
	d.Gateway = "10.10.20.1"
	d.VMIDRange = "" // the offset is undefined without it

	err := d.PreCreateCheck()
	if err == nil {
		t.Fatal("PreCreateCheck() = nil, want an error about --pve-vmid-range")
	}
	if !strings.Contains(err.Error(), "--pve-vmid-range") {
		t.Errorf("error = %q, want it to mention --pve-vmid-range", err)
	}
}

// A subnet smaller than the VMID range is a legitimate config: the subnet caps
// how many machines the pool can hold, and NextFreeVMID fills from the bottom
// of the range. A /30 with a 100-wide VMID range must save cleanly and run two
// nodes — requiring full coverage made small subnets unusable.
func TestPreCreateCheckAcceptsSubnetSmallerThanVMIDRange(t *testing.T) {
	d := newValidatedDriver()
	d.CloudInit = true
	d.IPMode = "static"
	d.IPStart = "10.10.20.9"
	d.IPEnd = "10.10.20.10"
	d.IPPrefix = "24"
	d.Gateway = "10.10.20.10"
	d.VMIDRange = "200-299"

	if err := d.PreCreateCheck(); err != nil {
		t.Fatalf("PreCreateCheck() returned error: %v", err)
	}
}

// The capacity limit is enforced per machine instead, once the VMID is known.
func TestStaticPoolExhaustionIsReportedPerMachine(t *testing.T) {
	d := newValidatedDriver()
	d.CloudInit = true
	d.IPMode = "static"
	d.IPStart = "10.10.20.9"
	d.IPEnd = "10.10.20.10"
	d.IPPrefix = "24"
	d.Gateway = "10.10.20.10"
	d.VMIDRange = "200-299"

	// .9 on a /30 (.8-.11) leaves .9 and .10 usable, so two machines fit.
	for vmid, want := range map[int]string{
		200: "ip=10.10.20.9/24,gw=10.10.20.10",
		201: "ip=10.10.20.10/24,gw=10.10.20.10",
	} {
		d.VMID = vmid
		got, err := d.resolveIPConfig()
		if err != nil {
			t.Fatalf("resolveIPConfig() for VMID %d returned error: %v", vmid, err)
		}
		if got != want {
			t.Errorf("resolveIPConfig() for VMID %d = %q, want %q", vmid, got, want)
		}
	}

	// The third machine has no address left.
	d.VMID = 202
	_, err := d.resolveIPConfig()
	if err == nil {
		t.Fatal("resolveIPConfig() = nil error for the third machine, want pool exhaustion")
	}
	if !strings.Contains(err.Error(), "static IP pool exhausted") {
		t.Errorf("error = %q, want it to report pool exhaustion", err)
	}
	if !strings.Contains(err.Error(), "holds 2 machines") {
		t.Errorf("error = %q, want it to state the capacity so the operator can act on it", err)
	}
}

func TestPreCreateCheckRejectsStaticFieldsInDHCPMode(t *testing.T) {
	d := newValidatedDriver()
	d.IPMode = "dhcp"
	d.IPStart = "10.10.20.10"
	d.IPEnd = "10.10.20.109"
	d.IPPrefix = "24"

	err := d.PreCreateCheck()
	if err == nil {
		t.Fatal("PreCreateCheck() = nil, want an error about the pool fields in dhcp mode")
	}
	if !strings.Contains(err.Error(), "--pve-ip-start") {
		t.Errorf("error = %q, want it to mention --pve-ip-start", err)
	}
}

// nameserver and searchdomain are cloud-init options, so with cloud-init off
// PVE would never receive them. Say so rather than dropping them silently.
func TestPreCreateCheckRejectsDNSWithoutCloudInit(t *testing.T) {
	d := newValidatedDriver()
	d.CloudInit = false
	d.Nameservers = "1.1.1.1"

	err := d.PreCreateCheck()
	if err == nil {
		t.Fatal("PreCreateCheck() = nil, want an error about cloud-init")
	}
	if !strings.Contains(err.Error(), "--pve-cloudinit") {
		t.Errorf("error = %q, want it to mention --pve-cloudinit", err)
	}
}

func TestPreCreateCheckAcceptsValidStaticConfig(t *testing.T) {
	d := newValidatedDriver()
	d.CloudInit = true
	d.IPMode = "static"
	d.IPStart = "10.10.20.10"
	d.IPEnd = "10.10.20.109"
	d.IPPrefix = "24"
	d.Gateway = "10.10.20.1"
	d.Nameservers = "10.10.20.1, 1.1.1.1"
	d.SearchDomain = "cluster.lan"
	d.VMIDRange = "200-299"

	if err := d.PreCreateCheck(); err != nil {
		t.Fatalf("PreCreateCheck() returned error: %v", err)
	}
	// Normalisation happens during validation so Configure can use it directly.
	if d.Nameservers != "10.10.20.1 1.1.1.1" {
		t.Errorf("Nameservers = %q, want the normalized space-separated form", d.Nameservers)
	}
}

// The address must be derived from the VMID the machine actually got.
// cloneFromTemplate retries when it loses a VMID claim, so computing this
// before the claim settles would assign an address belonging to another node.
func TestResolveIPConfigUsesFinalVMID(t *testing.T) {
	d := newValidatedDriver()
	d.CloudInit = true
	d.IPMode = "static"
	d.IPStart = "10.10.20.10"
	d.IPEnd = "10.10.20.109"
	d.IPPrefix = "24"
	d.Gateway = "10.10.20.1"
	d.VMIDRange = "200-299"

	// Simulates losing the claim on 200 and 201 before landing on 202.
	d.VMID = 202

	got, err := d.resolveIPConfig()
	if err != nil {
		t.Fatalf("resolveIPConfig() returned error: %v", err)
	}
	if got != "ip=10.10.20.12/24,gw=10.10.20.1" {
		t.Errorf("resolveIPConfig() = %q, want the address for VMID 202", got)
	}
}

// parseVMIDRange("") returns (0, 0, nil), so without an explicit guard a static
// pool with no range would compute its offset from VMID 0 and quietly hand out
// an address unrelated to the base. validateAddressing rejects this first, but
// resolveIPConfig must not depend on a gate in another function.
func TestResolveIPConfigStaticWithoutVMIDRange(t *testing.T) {
	d := newValidatedDriver()
	d.CloudInit = true
	d.IPMode = "static"
	d.IPStart = "10.10.20.10"
	d.IPEnd = "10.10.20.109"
	d.IPPrefix = "24"
	d.Gateway = "10.10.20.1"
	d.VMIDRange = ""
	d.VMID = 202

	got, err := d.resolveIPConfig()
	if err == nil {
		t.Fatalf("resolveIPConfig() = %q, want an error about --pve-vmid-range", got)
	}
	if !strings.Contains(err.Error(), "--pve-vmid-range") {
		t.Errorf("error = %q, want it to name --pve-vmid-range", err)
	}
}

func TestResolveIPConfigDHCP(t *testing.T) {
	d := newValidatedDriver()
	d.IPMode = "dhcp"
	d.VMID = 202

	got, err := d.resolveIPConfig()
	if err != nil {
		t.Fatalf("resolveIPConfig() returned error: %v", err)
	}
	if got != "ip=dhcp" {
		t.Errorf("resolveIPConfig() = %q, want ip=dhcp", got)
	}
}
