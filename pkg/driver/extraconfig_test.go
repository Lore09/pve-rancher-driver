package driver

import (
	"strings"
	"testing"

	"github.com/lore09/pve-rancher-driver/pkg/proxmox"
)

func TestParseExtraConfig(t *testing.T) {
	reserved := map[string]string{"cores": "--pve-cores"}

	t.Run("keeps order and splits on the first equals only", func(t *testing.T) {
		got, err := ParseExtraConfig([]string{
			"cpu=host",
			"  ",
			"hostpci0=0000:01:00,pcie=1",
			"startup=order=1,up=30",
		}, reserved)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []proxmox.ConfigOption{
			{Key: "cpu", Value: "host"},
			{Key: "hostpci0", Value: "0000:01:00,pcie=1"},
			{Key: "startup", Value: "order=1,up=30"},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d options, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("option %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	for name, tc := range map[string]struct {
		entry string
		want  string
	}{
		"no equals":       {"cpuhost", "not a key=value pair"},
		"empty key":       {"=host", "not a key=value pair"},
		"bad key":         {"CPU-Type=host", "not a valid PVE config key"},
		"empty value":     {"cpu=", "empty value"},
		"reserved key":    {"cores=8", "the driver writes that key itself from --pve-cores"},
		"newline injects": {"args=-a\n-b", "single line"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseExtraConfig([]string{tc.entry}, reserved)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}

	t.Run("duplicate key", func(t *testing.T) {
		_, err := ParseExtraConfig([]string{"cpu=host", "cpu=kvm64"}, reserved)
		if err == nil || !strings.Contains(err.Error(), "more than once") {
			t.Fatalf("got %v, want a duplicate-key error", err)
		}
	})
}

func TestReservedConfigKeys(t *testing.T) {
	d := &Driver{BootDiskDevice: "scsi0"}

	// Nothing is written to the NIC or the boot disk in this configuration, so
	// setting them by hand is legitimate rather than a collision.
	if _, taken := d.reservedConfigKeys()["net0"]; taken {
		t.Error("net0 reserved without --pve-net-bridge")
	}
	if _, taken := d.reservedConfigKeys()["scsi0"]; taken {
		t.Error("boot disk reserved without --pve-boot-disk-size")
	}

	d.NetBridge = "vmbr1"
	d.NetDevice = "net1"
	d.BootDiskGB = 40
	d.CloudInit = true
	d.DataDisks = []proxmox.DiskSpec{{Device: "scsi5"}, {}}

	keys := d.reservedConfigKeys()
	for _, key := range []string{"net1", "scsi0", "scsi5", "sshkeys", "cicustom", "agent"} {
		if _, taken := keys[key]; !taken {
			t.Errorf("%s should be reserved", key)
		}
	}
	if _, taken := keys["net0"]; taken {
		t.Error("net0 reserved while --pve-net-device is net1")
	}
}

func TestValidateCICustom(t *testing.T) {
	for name, tc := range map[string]struct {
		raw      string
		staticIP bool
		want     string
	}{
		"empty":            {"", false, ""},
		"vendor":           {"vendor=local:snippets/rancher.yaml", true, ""},
		"vendor and meta":  {"vendor=local:snippets/a.yaml,meta=cephfs:snippets/b.yml", false, ""},
		"network on dhcp":  {"network=local:snippets/net.yaml", false, ""},
		"network onstatic": {"network=local:snippets/net.yaml", true, "--pve-ip-mode=static"},
		"user rejected":    {"user=local:snippets/u.yaml", false, "Use vendor=... instead"},
		"unknown type":     {"boot=local:snippets/u.yaml", false, "is not one of"},
		"duplicate type":   {"vendor=local:snippets/a.yaml,vendor=local:snippets/b.yaml", false, "twice"},
		"not a pair":       {"local:snippets/a.yaml", false, "not a <type>=<volume> pair"},
		"missing snippets": {"vendor=local:rancher.yaml", false, "snippets/"},
		"bare path":        {"vendor=/etc/rancher.yaml", false, "snippets/"},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateCICustom(tc.raw, tc.staticIP)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
