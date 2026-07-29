package proxmox

import (
	"fmt"
	"strings"
	"testing"
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
