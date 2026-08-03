package driver

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/state"
)

func flagName(f mcnflag.Flag) string {
	switch v := f.(type) {
	case mcnflag.StringFlag:
		return v.Name
	case mcnflag.IntFlag:
		return v.Name
	case mcnflag.BoolFlag:
		return v.Name
	case mcnflag.StringSliceFlag:
		return v.Name
	default:
		return ""
	}
}

func TestParseNetFirewall(t *testing.T) {
	truthy := []string{"true", "1", "yes", "on", "TRUE", " True "}
	for _, s := range truthy {
		got, err := parseNetFirewall(s)
		if err != nil {
			t.Fatalf("parseNetFirewall(%q) returned error: %v", s, err)
		}
		if got == nil || !*got {
			t.Errorf("parseNetFirewall(%q) = %v, want pointer to true", s, got)
		}
	}

	falsy := []string{"false", "0", "no", "off", "FALSE"}
	for _, s := range falsy {
		got, err := parseNetFirewall(s)
		if err != nil {
			t.Fatalf("parseNetFirewall(%q) returned error: %v", s, err)
		}
		if got == nil || *got {
			t.Errorf("parseNetFirewall(%q) = %v, want pointer to false", s, got)
		}
	}

	// The empty string is the third state: leave PVE's own default alone.
	got, err := parseNetFirewall("")
	if err != nil {
		t.Fatalf("parseNetFirewall(%q) returned error: %v", "", err)
	}
	if got != nil {
		t.Errorf("parseNetFirewall(%q) = %v, want nil", "", got)
	}

	for _, s := range []string{"maybe", "2", "enabled"} {
		if _, err := parseNetFirewall(s); err == nil {
			t.Errorf("parseNetFirewall(%q) = nil error, want a validation error", s)
		}
	}
}

// The pve-net-* knobs are only applied while rewriting the net device, which
// only happens when a bridge is named. PreCreateCheck must reject the
// combinations that would otherwise be silently ignored.
func TestPreCreateCheckRejectsNetOptionsWithoutBridge(t *testing.T) {
	base := func() *Driver {
		return &Driver{
			APIUrl:         "https://pve.example:8006/api2/json",
			APITokenID:     "rancher@pve!machine",
			APITokenSecret: "secret",
			TemplateVMID:   9000,
			SkipPermCheck:  true,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Driver)
		wantErr bool
	}{
		{"vlan tag without bridge", func(d *Driver) { d.NetVlanTag = 42 }, true},
		{"mtu without bridge", func(d *Driver) { d.NetMTU = 9000 }, true},
		{"firewall without bridge", func(d *Driver) { d.NetFirewall = "true" }, true},
		{"invalid firewall value", func(d *Driver) { d.NetBridge = "vmbr0"; d.NetFirewall = "maybe" }, true},
		{"vlan tag with bridge is fine", func(d *Driver) { d.NetBridge = "vmbr0"; d.NetVlanTag = 42 }, false},
		{"no net options at all is fine", func(d *Driver) {}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base()
			tt.mutate(d)
			err := d.PreCreateCheck()
			if tt.wantErr && err == nil {
				t.Fatalf("PreCreateCheck() = nil, want an error")
			}
			// When no error is wanted we cannot assert err == nil: PreCreateCheck
			// goes on to dial the (nonexistent) PVE API. Assert only that it got
			// past validation, i.e. failed for a different reason.
			if !tt.wantErr && err != nil {
				for _, s := range []string{"--pve-net-vlan-tag", "--pve-net-mtu", "--pve-net-firewall"} {
					if strings.Contains(err.Error(), s) {
						t.Fatalf("PreCreateCheck() failed validation unexpectedly: %v", err)
					}
				}
			}
		})
	}
}

func TestPreCreateCheckDataDiskValidation(t *testing.T) {
	base := func() *Driver {
		return &Driver{
			APIUrl:         "https://pve.example:8006/api2/json",
			APITokenID:     "rancher@pve!machine",
			APITokenSecret: "secret",
			TemplateVMID:   9000,
			SkipPermCheck:  true,
			CloudInit:      true,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Driver)
		wantMsg string
	}{
		{
			name:    "invalid entry is rejected",
			mutate:  func(d *Driver) { d.DataDiskEntries = []string{"size=10"} },
			wantMsg: "storage is required",
		},
		{
			name: "mounting disk without cloud-init is rejected",
			mutate: func(d *Driver) {
				d.CloudInit = false
				d.DataDiskEntries = []string{"size=10,storage=local-lvm,fs=ext4,mount=/data"}
			},
			wantMsg: "--pve-cloudinit",
		},
		{
			name: "fs=none disk without cloud-init is allowed",
			mutate: func(d *Driver) {
				d.CloudInit = false
				d.DataDiskEntries = []string{"size=10,storage=local-lvm,fs=none"}
			},
			wantMsg: "",
		},
		{
			name:    "valid mounting disk with cloud-init is allowed",
			mutate:  func(d *Driver) { d.DataDiskEntries = []string{"size=10,storage=local-lvm,fs=ext4,mount=/data"} },
			wantMsg: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base()
			tt.mutate(d)
			err := d.PreCreateCheck()
			if tt.wantMsg != "" {
				if err == nil {
					t.Fatalf("PreCreateCheck() = nil error, want one mentioning %q", tt.wantMsg)
				}
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Fatalf("PreCreateCheck() error = %q, want it to mention %q", err, tt.wantMsg)
				}
				return
			}
			// PreCreateCheck goes on to dial a nonexistent PVE API, so a nil error
			// is not expected. Assert only that it got past disk validation.
			if err != nil {
				for _, s := range []string{"--pve-data-disk", "--pve-cloudinit"} {
					if strings.Contains(err.Error(), s) {
						t.Fatalf("PreCreateCheck() failed disk validation unexpectedly: %v", err)
					}
				}
			}
		})
	}
}

// PreCreateCheck must leave the parsed specs on the driver so Create does not
// have to parse the entries a second time.
func TestPreCreateCheckStoresParsedDataDisks(t *testing.T) {
	d := &Driver{
		APIUrl:          "https://pve.example:8006/api2/json",
		APITokenID:      "rancher@pve!machine",
		APITokenSecret:  "secret",
		TemplateVMID:    9000,
		SkipPermCheck:   true,
		CloudInit:       true,
		DataDiskEntries: []string{"size=10,storage=local-lvm,fs=ext4,mount=/data"},
	}
	_ = d.PreCreateCheck()
	if len(d.DataDisks) != 1 || d.DataDisks[0].Label != "pvedata1" {
		t.Fatalf("PreCreateCheck() left DataDisks = %+v, want one spec labelled pvedata1", d.DataDisks)
	}
}

func TestGetCreateFlagsContainsDataDiskFlags(t *testing.T) {
	names := map[string]bool{}
	for _, f := range (&Driver{}).GetCreateFlags() {
		names[f.String()] = true
	}
	for _, want := range []string{"pve-data-disk", "pve-boot-disk-size", "pve-disk-setup-timeout"} {
		if !names[want] {
			t.Errorf("GetCreateFlags() is missing %q", want)
		}
	}
	for _, gone := range []string{"pve-disk", "pve-extra-disk-size", "pve-extra-disk-storage"} {
		if names[gone] {
			t.Errorf("GetCreateFlags() still declares removed flag %q", gone)
		}
	}
}

func TestResolveVMName(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		machine string
		want    string
	}{
		{"no prefix keeps the machine name", "", "pool1-abc12-xyz", "pool1-abc12-xyz"},
		{"prefix is prepended with a hyphen", "k8s", "pool1-abc12-xyz", "k8s-pool1-abc12-xyz"},
		{"whitespace around the prefix is ignored", "  k8s  ", "node1", "k8s-node1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Driver{VMNamePrefix: tt.prefix}
			d.BaseDriver = &drivers.BaseDriver{MachineName: tt.machine}
			if got := d.resolveVMName(); got != tt.want {
				t.Errorf("resolveVMName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The prefix ends up in a PVE VM name, which PVE validates as a DNS name. A
// bad prefix must fail before a VM is cloned, not after.
func TestPreCreateCheckVMNamePrefix(t *testing.T) {
	base := func(prefix string) *Driver {
		d := &Driver{
			APIUrl:         "https://pve.example:8006/api2/json",
			APITokenID:     "rancher@pve!machine",
			APITokenSecret: "secret",
			TemplateVMID:   9000,
			SkipPermCheck:  true,
			VMNamePrefix:   prefix,
		}
		d.BaseDriver = &drivers.BaseDriver{MachineName: "pool1-abc12"}
		return d
	}

	bad := []struct {
		name    string
		prefix  string
		wantMsg string
	}{
		{"underscore is not a DNS character", "my_cluster", "letters, digits"},
		{"leading hyphen", "-k8s", "letters, digits"},
		{"trailing hyphen", "k8s-", "letters, digits"},
		{"dot is not allowed", "k8s.prod", "letters, digits"},
		{"space", "my cluster", "letters, digits"},
		{"too long for a DNS label", strings.Repeat("a", 60), "63"},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			err := base(tt.prefix).PreCreateCheck()
			if err == nil {
				t.Fatalf("PreCreateCheck() = nil error, want one mentioning %q", tt.wantMsg)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("PreCreateCheck() error = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}

	for _, prefix := range []string{"", "k8s", "prod1", "a", "my-cluster"} {
		t.Run("accepts "+prefix, func(t *testing.T) {
			err := base(prefix).PreCreateCheck()
			if err != nil && strings.Contains(err.Error(), "--pve-vm-name-prefix") {
				t.Fatalf("PreCreateCheck() rejected valid prefix %q: %v", prefix, err)
			}
		})
	}
}

// Cloud-init installs the SSH keys for `ciuser`, so that account and the one
// libmachine logs in as must be the same. When only ssh-user is given, the
// driver must derive ciuser from it rather than leaving cloud-init to guess.
func TestResolveCIUser(t *testing.T) {
	tests := []struct {
		name    string
		ciuser  string
		sshUser string
		want    string
	}{
		{"derived from ssh user when unset", "", "debian", "debian"},
		{"whitespace-only ciuser is treated as unset", "   ", "rancher", "rancher"},
		{"explicit ciuser wins over ssh user", "custom", "debian", "custom"},
		{"ciuser is trimmed", "  rancher  ", "debian", "rancher"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Driver{CIUser: tt.ciuser}
			d.BaseDriver = &drivers.BaseDriver{SSHUser: tt.sshUser}
			if got := d.resolveCIUser(); got != tt.want {
				t.Errorf("resolveCIUser() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Rancher derives a machine-config field name from a flag by splitting on the
// first dash and discarding what precedes it, assuming that part is the driver
// name. A flag named `ssh-port` therefore becomes the field `port`, which
// Rancher passes back as `--pve-port` — a flag this driver does not define, so
// provisioning dies with "flag provided but not defined: -pve-port".
//
// Every flag must therefore carry the pve- prefix. This is invisible until a
// real cluster is provisioned, which is why it is asserted here.
func TestAllCreateFlagsUsePveDriverPrefix(t *testing.T) {
	for _, f := range (&Driver{}).GetCreateFlags() {
		name := f.String()
		if !strings.HasPrefix(name, "pve-") {
			t.Errorf("flag %q lacks the pve- prefix: Rancher would rename it to %q and pass back --pve-%s",
				name, strings.SplitN(name, "-", 2)[1], strings.SplitN(name, "-", 2)[1])
		}
	}
}

// The field names Rancher derives are what the UI extension binds to, so they
// are part of the contract with pve-rancher-ui-extension.
func TestCreateFlagsDeriveExpectedFieldNames(t *testing.T) {
	// Rancher: strip up to the first dash, then lower-camel-case the rest.
	fieldName := func(flag string) string {
		rest := strings.SplitN(flag, "-", 2)[1]
		parts := strings.Split(rest, "-")
		out := parts[0]
		for _, p := range parts[1:] {
			if p != "" {
				out += strings.ToUpper(p[:1]) + p[1:]
			}
		}
		return out
	}

	got := map[string]bool{}
	for _, f := range (&Driver{}).GetCreateFlags() {
		got[fieldName(f.String())] = true
	}

	for _, want := range []string{
		"templateVmid", "dataDisk", "netBridge", "cloudinit",
		"sshUser", "sshPort", "vmNamePrefix", "bootDiskSize",
	} {
		if !got[want] {
			t.Errorf("no flag derives the machine-config field %q that the UI extension binds to", want)
		}
	}
}

// PVE validates `sshkeys` against [-%a-zA-Z0-9_.!~*'()], which excludes "+".
// url.QueryEscape emits "+" for spaces, and an SSH key always has spaces, so
// form encoding is rejected with "invalid urlencoded string".
func TestPVEURLEncode(t *testing.T) {
	const key = "ssh-rsa AAAAB3NzaC1yc2E+a/b== user@host"

	got := pveURLEncode(key)

	if strings.Contains(got, "+") {
		t.Errorf("pveURLEncode(%q) = %q, must not contain '+'", key, got)
	}
	if !strings.Contains(got, "%20") {
		t.Errorf("pveURLEncode(%q) = %q, spaces should be percent-encoded", key, got)
	}

	// Everything emitted must be inside the set PVE accepts.
	for _, r := range got {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-%_.!~*'()", r):
		default:
			t.Errorf("pveURLEncode(%q) produced %q, which PVE's urlencoded format rejects", key, string(r))
		}
	}

	// And it must still round-trip to the original key.
	back, err := url.QueryUnescape(got)
	if err != nil {
		t.Fatalf("output is not valid percent-encoding: %v", err)
	}
	if back != key {
		t.Errorf("round-trip = %q, want %q", back, key)
	}
}

func TestPVEURLEncodeMultipleKeys(t *testing.T) {
	got := pveURLEncode("ssh-rsa AAAA one@host\nssh-ed25519 BBBB two@host")

	if strings.Contains(got, "+") {
		t.Errorf("encoded keys contain '+': %q", got)
	}
	if !strings.Contains(got, "%0A") {
		t.Errorf("newline between keys should be percent-encoded, got %q", got)
	}
}

// PVE reports "running"; libmachine's state.Running stringifies to "Running".
// Comparing them directly never matches, which made GetState report Error for
// a healthy VM and libmachine's post-create wait time out after 60 retries.
func TestPVEStatusToState(t *testing.T) {
	tests := []struct {
		status string
		want   state.State
	}{
		{"running", state.Running},
		{"RUNNING", state.Running},
		{" running ", state.Running},
		{"stopped", state.Stopped},
		{"paused", state.Paused},
		{"suspended", state.Paused},
		{"prelaunch", state.Starting},
		{"", state.Error},
		{"something-new", state.Error},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := pveStatusToState(tt.status); got != tt.want {
				t.Errorf("pveStatusToState(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestIPFlagsAreDeclared(t *testing.T) {
	d := NewDriver("test", "/tmp").(*Driver)

	want := map[string]bool{
		"pve-ip-mode":      false,
		"pve-ip-start":     false,
		"pve-ip-end":       false,
		"pve-ip-prefix":    false,
		"pve-gateway":      false,
		"pve-nameservers":  false,
		"pve-searchdomain": false,
	}
	for _, f := range d.GetCreateFlags() {
		name := flagName(f)
		if _, ok := want[name]; ok {
			want[name] = true
		}
		if name == "pve-ipconfig" || name == "pve-ip-base" {
			t.Errorf("%s must be removed: a single CIDR cannot express both the pool bounds and the node netmask, which is what made /28 mean two different things", name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("flag %q is not declared", name)
		}
	}
}

// dhcp is the default so that a pool created before this flag existed keeps
// working unchanged.
func TestIPModeDefaultsToDHCP(t *testing.T) {
	d := NewDriver("test", "/tmp").(*Driver)
	if d.IPMode != string(ipModeDHCP) {
		t.Errorf("NewDriver().IPMode = %q, want %q", d.IPMode, ipModeDHCP)
	}
}

// The delay is the only thing standing between a guest that opens SSH early
// and a bootstrap command that runs before the network is configured, so it
// defaults to non-zero — but 0 must stay explicitly selectable.
func TestProvisionDelayDefaultsToNonZero(t *testing.T) {
	d := NewDriver("test", "/tmp").(*Driver)
	if d.ProvisionDelay != defaultProvisionDelay {
		t.Errorf("NewDriver().ProvisionDelay = %v, want %v", d.ProvisionDelay, defaultProvisionDelay)
	}

	var declared *mcnflag.IntFlag

	for _, f := range d.GetCreateFlags() {
		if v, ok := f.(mcnflag.IntFlag); ok && v.Name == "pve-provision-delay" {
			flag := v
			declared = &flag
		}
	}
	if declared == nil {
		t.Fatal("flag \"pve-provision-delay\" is not declared")
	}
	if want := int(defaultProvisionDelay / time.Second); declared.Value != want {
		t.Errorf("pve-provision-delay default = %d, want %d", declared.Value, want)
	}
}

// pve-node pins a single node explicitly; pve-allowed-nodes hands the choice
// to the scheduler. Accepting both silently would mean one of them is
// quietly ignored, so PreCreateCheck must reject the combination.
func TestPreCreateCheckNodeAndAllowedNodesMutuallyExclusive(t *testing.T) {
	base := func() *Driver {
		return &Driver{
			APIUrl:         "https://pve.example:8006/api2/json",
			APITokenID:     "rancher@pve!machine",
			APITokenSecret: "secret",
			TemplateVMID:   9000,
			SkipPermCheck:  true,
		}
	}

	d := base()
	d.Node = "pve1"
	d.AllowedNodes = "pve1,pve2"
	if err := d.PreCreateCheck(); err == nil {
		t.Fatal("PreCreateCheck() = nil, want an error for pve-node with pve-allowed-nodes both set")
	}

	d = base()
	d.Node = "pve1"
	if err := d.PreCreateCheck(); err != nil {
		t.Errorf("PreCreateCheck() with only --pve-node set returned error: %v", err)
	}

	d = base()
	d.AllowedNodes = "pve1,pve2"
	if err := d.PreCreateCheck(); err != nil {
		t.Errorf("PreCreateCheck() with only --pve-allowed-nodes set returned error: %v", err)
	}
}

func TestParseNodeList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"pve1", []string{"pve1"}},
		{"pve1,pve2", []string{"pve1", "pve2"}},
		{" pve1 , pve2 ,, pve3 ", []string{"pve1", "pve2", "pve3"}},
	}
	for _, tt := range tests {
		got := parseNodeList(tt.in)
		if len(got) != len(tt.want) {
			t.Fatalf("parseNodeList(%q) = %v, want %v", tt.in, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseNodeList(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}

func TestNormalizeTags(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{"empty is fine", "", "", ""},
		{"single tag", "rancher", "rancher", ""},
		{"multiple tags join with semicolons", "rancher,prod", "rancher;prod", ""},
		{"whitespace around tags is trimmed", " rancher , prod ", "rancher;prod", ""},
		{"mixed case is lowercased", "Rancher,PROD", "rancher;prod", ""},
		{"blank entries between commas are dropped", "rancher,,prod,", "rancher;prod", ""},
		{"duplicate tags collapse to one", "rancher,rancher,Rancher", "rancher", ""},
		{"digits, underscore, plus, dot and hyphen are allowed", "k8s_1.2+beta-x", "k8s_1.2+beta-x", ""},
		{"a space inside a tag is rejected", "prod cluster", "", "prod cluster"},
		{"a character outside PVE's set is rejected", "prod!", "", "prod!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeTags(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("normalizeTags(%q) = %q, nil error, want an error mentioning %q", tt.in, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("normalizeTags(%q) error = %q, want it to mention %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeTags(%q) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("normalizeTags(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveDescription(t *testing.T) {
	d := &Driver{BaseDriver: &drivers.BaseDriver{MachineName: "pool-abc"}, TemplateVMID: 9000}
	got := d.resolveDescription()
	// The default exists to stop the clone from carrying the template's own
	// notes, so it has to name this machine and where it came from.
	for _, want := range []string{"pool-abc", "9000", "docker-machine-driver-pve"} {
		if !strings.Contains(got, want) {
			t.Errorf("resolveDescription() = %q, want it to mention %q", got, want)
		}
	}

	d.Description = "custom notes"
	if got := d.resolveDescription(); got != "custom notes" {
		t.Errorf("resolveDescription() = %q, want the explicit --pve-description value", got)
	}
}

func TestValidateTemplateSelection(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Driver)
		wantErr string
	}{
		{"neither vmid nor tag", func(d *Driver) {}, "--pve-template-tag"},
		{"both are mutually exclusive", func(d *Driver) {
			d.TemplateVMID = 9000
			d.TemplateTag = "rancher"
		}, "mutually exclusive"},
		{"vmid alone is fine", func(d *Driver) { d.TemplateVMID = 9000 }, ""},
		{"tag alone is fine", func(d *Driver) { d.TemplateTag = "rancher" }, ""},
		{"invalid tag", func(d *Driver) { d.TemplateTag = "Not A Tag!" }, "not a valid PVE tag"},
		{"invalid match policy", func(d *Driver) {
			d.TemplateTag = "rancher"
			d.TemplateMatch = "fuzzy"
		}, "--pve-template-tag-match"},
		{"exact match policy is accepted", func(d *Driver) {
			d.TemplateTag = "rancher"
			d.TemplateMatch = "exact"
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Driver{TemplateMatch: defaultTemplateMatch}
			tt.mutate(d)
			err := d.validateTemplateSelection()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTemplateSelection() returned error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateTemplateSelection() = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateTemplateSelection() error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateTemplateSelectionNormalizesTags(t *testing.T) {
	d := &Driver{TemplateTag: " Rancher , NODE ,rancher", TemplateMatch: defaultTemplateMatch}
	if err := d.validateTemplateSelection(); err != nil {
		t.Fatalf("validateTemplateSelection() returned error: %v", err)
	}
	if d.TemplateTag != "rancher,node" {
		t.Errorf("TemplateTag = %q, want %q (lowercased, trimmed, deduped)", d.TemplateTag, "rancher,node")
	}
}
