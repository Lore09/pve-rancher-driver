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
	// In dhcp mode the static fields are irrelevant and must be ignored, not
	// validated: a pool that switches static -> dhcp may still carry them.
	got, err := buildIPConfig(ipModeDHCP, "10.10.20.10/24", "10.10.20.1", 250, 200)
	if err != nil {
		t.Fatalf("buildIPConfig() returned error: %v", err)
	}
	if got != "ip=dhcp" {
		t.Errorf("buildIPConfig() = %q, want %q", got, "ip=dhcp")
	}

	got, err = buildIPConfig(ipModeDHCP, "", "", 0, 0)
	if err != nil {
		t.Fatalf("buildIPConfig() with empty static fields returned error: %v", err)
	}
	if got != "ip=dhcp" {
		t.Errorf("buildIPConfig() = %q, want %q", got, "ip=dhcp")
	}
}

func TestBuildIPConfigStatic(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		gw      string
		vmid    int
		vmidMin int
		want    string
		wantErr string
	}{
		{
			name: "the first VMID in the range gets the base address",
			base: "10.10.20.10/24", gw: "10.10.20.1", vmid: 200, vmidMin: 200,
			want: "ip=10.10.20.10/24,gw=10.10.20.1",
		},
		{
			name: "the offset is added to the base",
			base: "10.10.20.10/24", gw: "10.10.20.1", vmid: 202, vmidMin: 200,
			want: "ip=10.10.20.12/24,gw=10.10.20.1",
		},
		{
			name: "the offset carries across an octet boundary on a /16",
			base: "10.10.20.250/16", gw: "10.10.0.1", vmid: 210, vmidMin: 200,
			want: "ip=10.10.21.4/16,gw=10.10.0.1",
		},
		{
			name: "a VMID below the range minimum is rejected",
			base: "10.10.20.10/24", gw: "10.10.20.1", vmid: 199, vmidMin: 200,
			wantErr: "below",
		},
		{
			// The subnet, not the VMID range, caps the pool — so running off the
			// end is reported as exhaustion with a machine count, which is what
			// the operator can act on.
			name: "an address past the end of the subnet is pool exhaustion",
			base: "10.10.20.250/24", gw: "10.10.20.1", vmid: 210, vmidMin: 200,
			wantErr: "static IP pool exhausted: --pve-ip-base 10.10.20.250/24 leaves room for 5 machines",
		},
		{
			// .250 on a /24 leaves .250-.254 usable, so offset 5 would land on
			// the broadcast address and is caught by the capacity check first.
			name: "the address that would be the broadcast address is pool exhaustion",
			base: "10.10.20.250/24", gw: "10.10.20.1", vmid: 205, vmidMin: 200,
			wantErr: "needs offset 5",
		},
		{
			name: "the last usable host in the subnet is accepted",
			base: "10.10.20.250/24", gw: "10.10.20.1", vmid: 204, vmidMin: 200,
			want: "ip=10.10.20.254/24,gw=10.10.20.1",
		},
		{
			name: "the network address is rejected",
			base: "10.10.20.0/24", gw: "10.10.20.1", vmid: 200, vmidMin: 200,
			wantErr: "network address",
		},
		{
			name: "an IPv6 base is rejected",
			base: "2001:db8::10/64", gw: "2001:db8::1", vmid: 200, vmidMin: 200,
			wantErr: "IPv4",
		},
		{
			name: "a base without a prefix is rejected",
			base: "10.10.20.10", gw: "10.10.20.1", vmid: 200, vmidMin: 200,
			wantErr: "--pve-ip-base",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildIPConfig(ipModeStatic, tt.base, tt.gw, tt.vmid, tt.vmidMin)
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

// validateStaticSpan is a save-time sanity check on the base and gateway. It
// deliberately does NOT require the subnet to cover the whole VMID range:
// NextFreeVMID hands out the lowest free id, so machines cluster at the bottom
// of the range and the subnet only has to hold the machines that exist at once.
// Capacity is enforced per machine by buildIPConfig instead.
func TestValidateStaticSpan(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		gw      string
		lo, hi  int
		wantErr string
	}{
		{name: "a range that fits", base: "10.10.20.10/24", gw: "10.10.20.1", lo: 200, hi: 299},
		{name: "a range that exactly reaches the last host", base: "10.10.20.10/24", gw: "10.10.20.1", lo: 200, hi: 444},
		{
			// Accepted on purpose. A VMID range wider than the subnet is a normal
			// way to keep id headroom; the subnet simply caps how many machines
			// can exist. Requiring full coverage demanded a /25 to run three
			// nodes with a 100-wide range.
			name: "a VMID range wider than the subnet is accepted", base: "10.10.20.200/24", gw: "10.10.20.1", lo: 200, hi: 299,
		},
		{
			name: "a /30 base with a wide VMID range is accepted", base: "10.10.20.9/30", gw: "10.10.20.10", lo: 200, hi: 299,
		},
		{
			name: "a base that is the network address is rejected", base: "10.10.20.0/24", gw: "10.10.20.1", lo: 200, hi: 299,
			wantErr: "is the network address",
		},
		{
			name: "a base with no room before the end of the subnet is rejected", base: "10.10.20.255/24", gw: "10.10.20.1", lo: 200, hi: 299,
			wantErr: "is the broadcast address",
		},
		{
			name: "a gateway outside the subnet", base: "10.10.20.10/24", gw: "10.10.99.1", lo: 200, hi: 299,
			wantErr: "--pve-gateway 10.10.99.1 is outside the subnet",
		},
		{
			name: "a malformed gateway", base: "10.10.20.10/24", gw: "not-an-ip", lo: 200, hi: 299,
			wantErr: "--pve-gateway",
		},
		{
			name: "a prefix too small to hold hosts", base: "10.10.20.10/31", gw: "10.10.20.11", lo: 200, hi: 200,
			wantErr: "/30",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStaticSpan(tt.base, tt.gw, tt.lo, tt.hi)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("validateStaticSpan() = nil, want an error mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateStaticSpan() returned error: %v", err)
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
	d.IPBase = "10.10.20.10/24"
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
	d.IPBase = "10.10.20.9/30"
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
	d.IPBase = "10.10.20.9/30"
	d.Gateway = "10.10.20.10"
	d.VMIDRange = "200-299"

	// .9 on a /30 (.8-.11) leaves .9 and .10 usable, so two machines fit.
	for vmid, want := range map[int]string{
		200: "ip=10.10.20.9/30,gw=10.10.20.10",
		201: "ip=10.10.20.10/30,gw=10.10.20.10",
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
	if !strings.Contains(err.Error(), "room for 2 machines") {
		t.Errorf("error = %q, want it to state the capacity so the operator can act on it", err)
	}
}

func TestPreCreateCheckRejectsStaticFieldsInDHCPMode(t *testing.T) {
	d := newValidatedDriver()
	d.IPMode = "dhcp"
	d.IPBase = "10.10.20.10/24"

	err := d.PreCreateCheck()
	if err == nil {
		t.Fatal("PreCreateCheck() = nil, want an error about --pve-ip-base in dhcp mode")
	}
	if !strings.Contains(err.Error(), "--pve-ip-base") {
		t.Errorf("error = %q, want it to mention --pve-ip-base", err)
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
	d.IPBase = "10.10.20.10/24"
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
	d.IPBase = "10.10.20.10/24"
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
	d.IPBase = "10.10.20.10/24"
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
