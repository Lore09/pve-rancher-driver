package driver

import (
	"strings"
	"testing"

	"github.com/docker/machine/libmachine/drivers"
)

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
