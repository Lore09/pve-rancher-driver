package proxmox

import (
	"fmt"
	"strings"
	"testing"

	proxmox "github.com/luthermonson/go-proxmox"
)

func TestNetDeviceKey(t *testing.T) {
	if got := netDeviceKey(""); got != "net0" {
		t.Errorf("netDeviceKey(%q) = %q, want net0", "", got)
	}
	if got := netDeviceKey("net3"); got != "net3" {
		t.Errorf("netDeviceKey(%q) = %q, want net3", "net3", got)
	}
}

func TestBuildNetValue(t *testing.T) {
	enabled := true
	disabled := false

	tests := []struct {
		name string
		opts VMOptions
		want string
	}{
		{
			name: "bridge only defaults the model to virtio",
			opts: VMOptions{NetBridge: "vmbr0"},
			want: "model=virtio,bridge=vmbr0",
		},
		{
			name: "explicit model is honoured",
			opts: VMOptions{NetBridge: "vmbr0", NetModel: "e1000"},
			want: "model=e1000,bridge=vmbr0",
		},
		{
			name: "vlan tag is emitted when set",
			opts: VMOptions{NetBridge: "vmbr1", NetVlanTag: 42},
			want: "model=virtio,bridge=vmbr1,tag=42",
		},
		{
			name: "zero vlan tag is omitted so the NIC stays untagged",
			opts: VMOptions{NetBridge: "vmbr1", NetVlanTag: 0},
			want: "model=virtio,bridge=vmbr1",
		},
		{
			name: "mtu is emitted when set",
			opts: VMOptions{NetBridge: "vmbr0", NetMTU: 9000},
			want: "model=virtio,bridge=vmbr0,mtu=9000",
		},
		{
			name: "zero mtu is omitted so PVE picks the default",
			opts: VMOptions{NetBridge: "vmbr0", NetMTU: 0},
			want: "model=virtio,bridge=vmbr0",
		},
		{
			name: "firewall true renders as 1",
			opts: VMOptions{NetBridge: "vmbr0", NetFirewall: &enabled},
			want: "model=virtio,bridge=vmbr0,firewall=1",
		},
		{
			name: "firewall false renders as 0 rather than being dropped",
			opts: VMOptions{NetBridge: "vmbr0", NetFirewall: &disabled},
			want: "model=virtio,bridge=vmbr0,firewall=0",
		},
		{
			name: "nil firewall is omitted entirely",
			opts: VMOptions{NetBridge: "vmbr0", NetFirewall: nil},
			want: "model=virtio,bridge=vmbr0",
		},
		{
			name: "all options together keep PVE's key order",
			opts: VMOptions{
				NetBridge:   "vmbr2",
				NetModel:    "virtio",
				NetVlanTag:  100,
				NetMTU:      1450,
				NetFirewall: &enabled,
			},
			want: "model=virtio,bridge=vmbr2,tag=100,mtu=1450,firewall=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildNetValue(tt.opts); got != tt.want {
				t.Errorf("buildNetValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildDiskValue(t *testing.T) {
	tests := []struct {
		name string
		spec DiskSpec
		want string
	}{
		{
			name: "all options render in PVE key order",
			spec: DiskSpec{Size: 100, Storage: "local-lvm", Label: "pvedata1", Discard: "on", IOThread: "1", Backup: "0"},
			want: "local-lvm:100,serial=pvedata1,discard=on,iothread=1,backup=0",
		},
		{
			name: "discard off and backup on are rendered, not dropped",
			spec: DiskSpec{Size: 50, Storage: "ceph-rbd", Label: "pvedata2", Discard: "off", IOThread: "0", Backup: "1"},
			want: "ceph-rbd:50,serial=pvedata2,discard=off,iothread=0,backup=1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildDiskValue(tt.spec); got != tt.want {
				t.Errorf("buildDiskValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

// node builds a NodeStatus for the selectNode tests below. maxMemGB/memGB
// are in GB purely so test tables stay readable; freeMem works in bytes.
func node(name, status string, maxMemGB, memGB uint64) *proxmox.NodeStatus {
	const gb = 1024 * 1024 * 1024
	return &proxmox.NodeStatus{Node: name, Status: status, MaxMem: maxMemGB * gb, Mem: memGB * gb}
}

func TestSelectNode(t *testing.T) {
	tests := []struct {
		name    string
		nodes   proxmox.NodeStatuses
		allowed []string
		want    string
		wantErr string
	}{
		{
			// The only cluster shape this project's author can actually run
			// against — a single online node must "just work" exactly like
			// the old first-online-node logic did.
			name:  "single online node is chosen regardless of load",
			nodes: proxmox.NodeStatuses{node("pve1", "online", 32, 30)},
			want:  "pve1",
		},
		{
			name: "the node with the most free memory wins",
			nodes: proxmox.NodeStatuses{
				node("pve1", "online", 64, 60), // 4GB free
				node("pve2", "online", 64, 16), // 48GB free
				node("pve3", "online", 64, 40), // 24GB free
			},
			want: "pve2",
		},
		{
			name: "offline nodes are never candidates",
			nodes: proxmox.NodeStatuses{
				node("pve1", "offline", 64, 4), // most free, but down
				node("pve2", "online", 64, 32),
			},
			want: "pve2",
		},
		{
			name: "allowed restricts the candidate set even when excluded nodes are less loaded",
			nodes: proxmox.NodeStatuses{
				node("pve1", "online", 64, 4),
				node("pve2", "online", 64, 32),
			},
			allowed: []string{"pve2"},
			want:    "pve2",
		},
		{
			name:    "none of the allowed nodes online is an error naming them",
			nodes:   proxmox.NodeStatuses{node("pve1", "offline", 64, 4)},
			allowed: []string{"pve1"},
			wantErr: "pve1",
		},
		{
			name:    "no online node anywhere is an error",
			nodes:   proxmox.NodeStatuses{node("pve1", "offline", 64, 4)},
			wantErr: "no online",
		},
		{
			name: "a tie on free memory breaks on node name for a deterministic result",
			nodes: proxmox.NodeStatuses{
				node("pve-b", "online", 64, 32),
				node("pve-a", "online", 64, 32),
			},
			want: "pve-a",
		},
		{
			name:  "mem reported above maxmem floors free at zero rather than going negative",
			nodes: proxmox.NodeStatuses{node("pve1", "online", 32, 40)},
			want:  "pve1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectNode(tt.nodes, tt.allowed)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("selectNode() = %q, nil error, want an error mentioning %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("selectNode() error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectNode() returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("selectNode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAllocateDiskSlots(t *testing.T) {
	two := []DiskSpec{{Size: 10, Storage: "s"}, {Size: 20, Storage: "s"}}

	tests := []struct {
		name    string
		cfg     map[string]interface{}
		specs   []DiskSpec
		want    []string
		wantErr string
	}{
		{
			name:  "boot disk only leaves scsi1 and scsi2",
			cfg:   map[string]interface{}{"scsi0": "local-lvm:vm-100-disk-0", "ide2": "local-lvm:cloudinit"},
			specs: two,
			want:  []string{"scsi1", "scsi2"},
		},
		{
			name:  "occupied slots are skipped",
			cfg:   map[string]interface{}{"scsi0": "x", "scsi1": "x", "scsi3": "x"},
			specs: two,
			want:  []string{"scsi2", "scsi4"},
		},
		{
			name:  "explicit device is honoured and never reused by allocation",
			cfg:   map[string]interface{}{"scsi0": "x"},
			specs: []DiskSpec{{Size: 10, Storage: "s", Device: "scsi1"}, {Size: 10, Storage: "s"}},
			want:  []string{"scsi1", "scsi2"},
		},
		{
			name:    "explicit device that is already in use is an error",
			cfg:     map[string]interface{}{"scsi0": "x", "scsi5": "x"},
			specs:   []DiskSpec{{Size: 10, Storage: "s", Device: "scsi5"}},
			wantErr: "scsi5",
		},
		{
			name:    "running out of slots is an error",
			cfg:     fullSCSIConfig(),
			specs:   []DiskSpec{{Size: 10, Storage: "s"}},
			wantErr: "no free SCSI slot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := allocateDiskSlots(tt.cfg, tt.specs)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("allocateDiskSlots() = nil error, want one mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("allocateDiskSlots() error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("allocateDiskSlots() returned error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("allocateDiskSlots() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("slot %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// fullSCSIConfig returns a config map with every SCSI slot occupied.
func fullSCSIConfig() map[string]interface{} {
	cfg := map[string]interface{}{}
	for i := 0; i <= 30; i++ {
		cfg[fmt.Sprintf("scsi%d", i)] = "occupied"
	}
	return cfg
}

// A delete must succeed against a VM that is already gone, or Rancher retries
// the machine deletion forever.
func TestIsNotFound(t *testing.T) {
	notFound := []string{
		"proxmox: node \"pve\" not reachable: 500 Configuration file 'nodes/pve/qemu-server/480.conf' does not exist",
		"No such VM 480",
		"vm not found",
	}
	for _, msg := range notFound {
		if !IsNotFound(errString(msg)) {
			t.Errorf("IsNotFound(%q) = false, want true", msg)
		}
	}

	other := []string{
		"unable to destroy VM 480 - VM is running",
		"not authorized to access endpoint",
		"proxmox: cannot read vm 480 config: connection refused",
	}
	for _, msg := range other {
		if IsNotFound(errString(msg)) {
			t.Errorf("IsNotFound(%q) = true, want false — a real failure must not be swallowed", msg)
		}
	}

	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) = true, want false")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestSplitTags(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"rancher", []string{"rancher"}},
		// PVE stores tags semicolon-separated but accepts commas on input.
		{"rancher;node", []string{"rancher", "node"}},
		{"rancher,node", []string{"rancher", "node"}},
		{" Rancher ; NODE ", []string{"rancher", "node"}},
		{";;rancher;;", []string{"rancher"}},
	}
	for _, tt := range tests {
		got := splitTags(tt.in)
		if len(got) != len(tt.want) {
			t.Fatalf("splitTags(%q) = %v, want %v", tt.in, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitTags(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}

func TestTagsMatch(t *testing.T) {
	tests := []struct {
		name  string
		have  []string
		want  []string
		match TemplateMatch
		ok    bool
	}{
		{"subset: extra build tags are fine", []string{"rancher", "debian13", "2026-08"}, []string{"rancher"}, MatchSubset, true},
		{"subset: all requested must be present", []string{"rancher"}, []string{"rancher", "gpu"}, MatchSubset, false},
		{"subset: identical sets match", []string{"rancher"}, []string{"rancher"}, MatchSubset, true},
		{"exact: extra tags disqualify", []string{"rancher", "debian13"}, []string{"rancher"}, MatchExact, false},
		{"exact: identical sets match", []string{"rancher", "gpu"}, []string{"gpu", "rancher"}, MatchExact, true},
		{"untagged template never matches", nil, []string{"rancher"}, MatchSubset, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tagsMatch(tt.have, tt.want, tt.match); got != tt.ok {
				t.Errorf("tagsMatch(%v, %v, %q) = %v, want %v", tt.have, tt.want, tt.match, got, tt.ok)
			}
		})
	}
}

func TestCloneOptionsDropStorageAndFormatForLinkedClones(t *testing.T) {
	// PVE rejects storage/format on a linked clone. The driver validates this
	// first, but the client must not pass them on regardless of how it was
	// called — a linked clone has no storage of its own to land on.
	for _, tt := range []struct {
		name   string
		linked bool
		want   string
	}{
		{"full clone keeps the storage", false, "ceph-rbd"},
		{"linked clone drops it", true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := CloneOptions{Linked: tt.linked, Storage: "ceph-rbd", Format: "qcow2"}
			got := cloneParams(opts)
			if got.Storage != tt.want {
				t.Errorf("Storage = %q, want %q", got.Storage, tt.want)
			}
			wantFormat := "qcow2"
			if tt.linked {
				wantFormat = ""
			}
			if got.Format != wantFormat {
				t.Errorf("Format = %q, want %q", got.Format, wantFormat)
			}
			wantFull := uint8(1)
			if tt.linked {
				wantFull = 0
			}
			if got.Full != wantFull {
				t.Errorf("Full = %d, want %d", got.Full, wantFull)
			}
		})
	}
}

func TestSetDiskProperty(t *testing.T) {
	for name, tc := range map[string]struct {
		value, key, val, want string
	}{
		"appends when absent": {
			"local-lvm:vm-101-disk-0,iothread=1,size=20G", "backup", "0",
			"local-lvm:vm-101-disk-0,iothread=1,size=20G,backup=0",
		},
		"replaces in place, preserving order": {
			"local-lvm:vm-101-disk-0,backup=1,size=20G", "backup", "0",
			"local-lvm:vm-101-disk-0,backup=0,size=20G",
		},
		"volume id is never treated as a pair": {
			"local-lvm:vm-101-disk-0", "backup", "1",
			"local-lvm:vm-101-disk-0,backup=1",
		},
		// A volume id can itself contain the key as a substring; only a real
		// key=value pair after the first element may be replaced.
		"substring in the volume id is left alone": {
			"backups:vm-101-disk-0,size=20G", "backup", "0",
			"backups:vm-101-disk-0,size=20G,backup=0",
		},
		"no change when already correct": {
			"local-lvm:vm-101-disk-0,backup=1", "backup", "1",
			"local-lvm:vm-101-disk-0,backup=1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := setDiskProperty(tc.value, tc.key, tc.val); got != tc.want {
				t.Errorf("setDiskProperty(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}
