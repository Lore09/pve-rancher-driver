package driver

import (
	"strings"
	"testing"
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
