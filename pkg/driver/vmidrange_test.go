package driver

import (
	"strings"
	"testing"
)

func TestParseVMIDRange(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantMin int
		wantMax int
	}{
		{"empty means unset", "", 0, 0},
		{"simple range", "200-299", 200, 299},
		{"whitespace tolerated", "  200 - 299  ", 200, 299},
		{"single id range", "500-500", 500, 500},
		{"lowest permitted", "100-101", 100, 101},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMin, gotMax, err := parseVMIDRange(tt.in)
			if err != nil {
				t.Fatalf("parseVMIDRange(%q) returned error: %v", tt.in, err)
			}
			if gotMin != tt.wantMin || gotMax != tt.wantMax {
				t.Errorf("parseVMIDRange(%q) = %d,%d, want %d,%d", tt.in, gotMin, gotMax, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestParseVMIDRangeRejections(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantMsg string
	}{
		{"no separator", "200", "<min>-<max>"},
		{"non numeric min", "abc-299", "not a number"},
		{"non numeric max", "200-abc", "not a number"},
		{"reserved low id", "50-99", "reserved"},
		{"zero", "0-100", "reserved"},
		{"inverted", "299-200", "below the minimum"},
		{"above the ceiling", "100-1000000000", "highest usable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseVMIDRange(tt.in)
			if err == nil {
				t.Fatalf("parseVMIDRange(%q) = nil error, want one mentioning %q", tt.in, tt.wantMsg)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("parseVMIDRange(%q) error = %q, want it to mention %q", tt.in, err, tt.wantMsg)
			}
		})
	}
}

// An explicit VMID and a range cannot both be honoured, and silently ignoring
// one of them would be worse than refusing the pool.
func TestPreCreateCheckRejectsVMIDWithRange(t *testing.T) {
	d := &Driver{
		APIUrl:         "https://pve.example:8006/api2/json",
		APITokenID:     "rancher@pve!machine",
		APITokenSecret: "secret",
		TemplateVMID:   9000,
		SkipPermCheck:  true,
		VMID:           150,
		VMIDRange:      "200-299",
	}
	err := d.PreCreateCheck()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("PreCreateCheck() = %v, want a mutual-exclusion error", err)
	}
}

func TestIsVMIDTakenError(t *testing.T) {
	if !isVMIDTakenError(errString("unable to create VM 101 - VM 101 already exists on node 'pve'")) {
		t.Error("PVE's duplicate-id message should be recognised as a lost race")
	}
	if isVMIDTakenError(errString("proxmox: template 9000 not found")) {
		t.Error("an unrelated failure must not be retried as a lost race")
	}
	if isVMIDTakenError(nil) {
		t.Error("nil is not a lost race")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
