package proxmox

import "testing"

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
