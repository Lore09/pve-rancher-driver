package driver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lore09/pve-rancher-driver/pkg/proxmox"
)

func TestParseDataDisksDefaults(t *testing.T) {
	got, err := ParseDataDisks([]string{
		"size=100,storage=local-lvm,mount=/var/lib/longhorn",
		"size=50,storage=ceph-rbd,fs=xfs,mount=/var/lib/rancher,backup=1",
		"size=200,storage=local-lvm,fs=none",
	})
	if err != nil {
		t.Fatalf("ParseDataDisks() returned error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ParseDataDisks() returned %d specs, want 3", len(got))
	}

	want := []proxmox.DiskSpec{
		{Size: 100, Storage: "local-lvm", FS: "ext4", Mount: "/var/lib/longhorn", Label: "pvedata1", Discard: "on", IOThread: "1", Backup: "0"},
		{Size: 50, Storage: "ceph-rbd", FS: "xfs", Mount: "/var/lib/rancher", Label: "pvedata2", Discard: "on", IOThread: "1", Backup: "1"},
		{Size: 200, Storage: "local-lvm", FS: "none", Label: "pvedata3", Discard: "on", IOThread: "1", Backup: "0"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("spec %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseDataDisksOverrides(t *testing.T) {
	got, err := ParseDataDisks([]string{
		"size=10,storage=local-lvm,fs=ext4,mount=/data,label=mydata,device=scsi7,discard=off,iothread=0,backup=1",
	})
	if err != nil {
		t.Fatalf("ParseDataDisks() returned error: %v", err)
	}
	want := proxmox.DiskSpec{
		Size: 10, Storage: "local-lvm", FS: "ext4", Mount: "/data",
		Label: "mydata", Device: "scsi7", Discard: "off", IOThread: "0", Backup: "1",
	}
	if got[0] != want {
		t.Errorf("spec = %+v, want %+v", got[0], want)
	}
}

func TestParseDataDisksIgnoresBlankEntries(t *testing.T) {
	got, err := ParseDataDisks([]string{"", "  ", "size=10,storage=local-lvm,fs=none"})
	if err != nil {
		t.Fatalf("ParseDataDisks() returned error: %v", err)
	}
	// The label suffix is the position among the *kept* disks, so a blank entry
	// in the Rancher form must not shift the numbering.
	if len(got) != 1 || got[0].Label != "pvedata1" {
		t.Fatalf("ParseDataDisks() = %+v, want one spec labelled pvedata1", got)
	}
}

func TestParseDataDisksRejections(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		wantMsg string
	}{
		{"missing size", []string{"storage=local-lvm,fs=none"}, "size"},
		{"zero size", []string{"size=0,storage=local-lvm,fs=none"}, "size"},
		{"negative size", []string{"size=-5,storage=local-lvm,fs=none"}, "size"},
		{"non numeric size", []string{"size=big,storage=local-lvm,fs=none"}, "size"},
		{"missing storage", []string{"size=10,fs=none"}, "storage"},
		{"unknown key", []string{"size=10,storage=local-lvm,fs=none,colour=red"}, "colour"},
		{"malformed pair", []string{"size=10,storage"}, "key=value"},
		{"bad fs", []string{"size=10,storage=local-lvm,fs=btrfs,mount=/data"}, "fs"},
		{"mount missing", []string{"size=10,storage=local-lvm,fs=ext4"}, "mount"},
		{"mount with fs none", []string{"size=10,storage=local-lvm,fs=none,mount=/data"}, "mount"},
		{"relative mount", []string{"size=10,storage=local-lvm,fs=ext4,mount=data/x"}, "absolute"},
		{"unsafe mount", []string{"size=10,storage=local-lvm,fs=ext4,mount=/data;rm -rf /"}, "characters"},
		{"unsafe label", []string{"size=10,storage=local-lvm,fs=ext4,mount=/data,label=a$b"}, "characters"},
		{"bad device name", []string{"size=10,storage=local-lvm,fs=none,device=virtio1"}, "scsi1"},
		{"device out of range", []string{"size=10,storage=local-lvm,fs=none,device=scsi31"}, "scsi1"},
		{"boot slot requested", []string{"size=10,storage=local-lvm,fs=none,device=scsi0"}, "scsi1"},
		{"bad discard", []string{"size=10,storage=local-lvm,fs=none,discard=maybe"}, "discard"},
		{"bad iothread", []string{"size=10,storage=local-lvm,fs=none,iothread=2"}, "iothread"},
		{"bad backup", []string{"size=10,storage=local-lvm,fs=none,backup=yes"}, "backup"},
		{
			"duplicate mount",
			[]string{"size=10,storage=s,fs=ext4,mount=/data", "size=10,storage=s,fs=ext4,mount=/data"},
			"duplicate mount",
		},
		{
			"duplicate label",
			[]string{"size=10,storage=s,fs=none,label=d", "size=10,storage=s,fs=none,label=d"},
			"duplicate label",
		},
		{
			"duplicate device",
			[]string{"size=10,storage=s,fs=none,device=scsi3", "size=10,storage=s,fs=none,device=scsi3"},
			"duplicate device",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDataDisks(tt.entries)
			if err == nil {
				t.Fatalf("ParseDataDisks(%q) = nil error, want an error mentioning %q", tt.entries, tt.wantMsg)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("ParseDataDisks(%q) error = %q, want it to mention %q", tt.entries, err, tt.wantMsg)
			}
		})
	}
}

func TestParseDataDisksTooMany(t *testing.T) {
	entries := make([]string, 0, 31)
	for i := 0; i < 31; i++ {
		entries = append(entries, "size=1,storage=local-lvm,fs=none")
	}
	if _, err := ParseDataDisks(entries); err == nil {
		t.Fatal("ParseDataDisks() = nil error for 31 disks, want an error")
	}
}

func TestNeedsGuestSetup(t *testing.T) {
	if (proxmox.DiskSpec{FS: "ext4"}).NeedsGuestSetup() != true {
		t.Error("ext4 disk should need guest setup")
	}
	if (proxmox.DiskSpec{FS: proxmox.FSNone}).NeedsGuestSetup() != false {
		t.Error("fs=none disk should not need guest setup")
	}
}

func TestParseDataDisksNormalizesMount(t *testing.T) {
	tests := []struct {
		entry string
		want  string
	}{
		{"size=10,storage=s,fs=ext4,mount=/data/", "/data"},
		{"size=10,storage=s,fs=ext4,mount=/data//sub//", "/data/sub"},
		{"size=10,storage=s,fs=ext4,mount=/var/lib/longhorn", "/var/lib/longhorn"},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			got, err := ParseDataDisks([]string{tt.entry})
			if err != nil {
				t.Fatalf("ParseDataDisks() returned error: %v", err)
			}
			if got[0].Mount != tt.want {
				t.Errorf("mount = %q, want %q", got[0].Mount, tt.want)
			}
		})
	}
}

// Mounting a freshly formatted disk over a system directory breaks the node in
// ways that look like a Rancher fault, so these must never reach a guest.
func TestParseDataDisksRejectsSystemMounts(t *testing.T) {
	forbidden := []string{
		"/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64",
		"/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var",
		"/etc/kubernetes", "/usr/local", "/dev/sdb", "/proc/self", "/boot/efi",
	}
	for _, mount := range forbidden {
		t.Run(mount, func(t *testing.T) {
			_, err := ParseDataDisks([]string{"size=10,storage=s,fs=ext4,mount=" + mount})
			if err == nil {
				t.Fatalf("ParseDataDisks(mount=%q) = nil error, want a rejection", mount)
			}
		})
	}

	// Trailing slashes and duplicate separators must not smuggle one through.
	for _, mount := range []string{"/", "//", "/var/", "/etc//"} {
		t.Run("normalized "+mount, func(t *testing.T) {
			if _, err := ParseDataDisks([]string{"size=10,storage=s,fs=ext4,mount=" + mount}); err == nil {
				t.Fatalf("ParseDataDisks(mount=%q) = nil error, want a rejection", mount)
			}
		})
	}
}

func TestParseDataDisksAllowsSensibleMounts(t *testing.T) {
	allowed := []string{
		"/var/lib/longhorn", "/var/lib/rancher", "/data", "/mnt/data",
		"/opt/data", "/srv/storage", "/var/lib/docker",
	}
	for _, mount := range allowed {
		t.Run(mount, func(t *testing.T) {
			if _, err := ParseDataDisks([]string{"size=10,storage=s,fs=ext4,mount=" + mount}); err != nil {
				t.Errorf("ParseDataDisks(mount=%q) rejected a valid mount: %v", mount, err)
			}
		})
	}
}

func TestParseDataDisksRejectsTraversal(t *testing.T) {
	for _, mount := range []string{"/data/../etc", "/var/lib/../../usr", "/.."} {
		t.Run(mount, func(t *testing.T) {
			if _, err := ParseDataDisks([]string{"size=10,storage=s,fs=ext4,mount=" + mount}); err == nil {
				t.Fatalf("ParseDataDisks(mount=%q) = nil error, want a rejection", mount)
			}
		})
	}
}

func TestParseDataDisksRejectsNestedMounts(t *testing.T) {
	tests := [][]string{
		{"size=10,storage=s,fs=ext4,mount=/data", "size=10,storage=s,fs=ext4,mount=/data/sub"},
		{"size=10,storage=s,fs=ext4,mount=/data/sub", "size=10,storage=s,fs=ext4,mount=/data"},
	}
	for i, entries := range tests {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			_, err := ParseDataDisks(entries)
			if err == nil {
				t.Fatal("ParseDataDisks() = nil error for nested mounts, want a rejection")
			}
			if !strings.Contains(err.Error(), "nested") {
				t.Errorf("error = %q, want it to mention nesting", err)
			}
		})
	}

	// Sibling paths that merely share a prefix are fine.
	if _, err := ParseDataDisks([]string{
		"size=10,storage=s,fs=ext4,mount=/data1",
		"size=10,storage=s,fs=ext4,mount=/data2",
	}); err != nil {
		t.Errorf("sibling mounts rejected: %v", err)
	}
}

// Normalization must happen before the duplicate check, or /data and /data/
// both pass while naming the same mount point.
func TestParseDataDisksDuplicateAfterNormalization(t *testing.T) {
	_, err := ParseDataDisks([]string{
		"size=10,storage=s,fs=ext4,mount=/data",
		"size=10,storage=s,fs=ext4,mount=/data/",
	})
	if err == nil {
		t.Fatal("ParseDataDisks() = nil error, want a duplicate-mount rejection")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want it to mention a duplicate", err)
	}
}
