package driver

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lore09/pve-rancher-driver/pkg/proxmox"
)

// The rendered script is compared against a golden file: it runs as root in
// every provisioned node, so any change to it must show up as a reviewable
// diff rather than hiding inside a string builder.
func TestRenderDiskSetupScriptGolden(t *testing.T) {
	disks := []proxmox.AttachedDisk{
		{Device: "scsi1", Spec: proxmox.DiskSpec{Size: 100, Storage: "local-lvm", FS: "ext4", Mount: "/var/lib/longhorn", Label: "pvedata1"}},
		{Device: "scsi2", Spec: proxmox.DiskSpec{Size: 50, Storage: "ceph-rbd", FS: "xfs", Mount: "/var/lib/rancher", Label: "pvedata2"}},
	}

	got := renderDiskSetupScript(disks)

	want, err := os.ReadFile("testdata/disk-setup.golden.sh")
	if err != nil {
		t.Fatalf("cannot read golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("renderDiskSetupScript() does not match testdata/disk-setup.golden.sh.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderDiskSetupScriptSkipsUnformattedDisks(t *testing.T) {
	got := renderDiskSetupScript([]proxmox.AttachedDisk{
		{Device: "scsi1", Spec: proxmox.DiskSpec{Size: 10, Storage: "s", FS: proxmox.FSNone, Label: "pvedata1"}},
	})
	if strings.Contains(got, "mkfs") {
		t.Errorf("fs=none disk must not be formatted, got:\n%s", got)
	}
}

func TestRenderDiskSetupScriptIsIdempotent(t *testing.T) {
	got := renderDiskSetupScript([]proxmox.AttachedDisk{
		{Device: "scsi1", Spec: proxmox.DiskSpec{Size: 10, Storage: "s", FS: "ext4", Mount: "/data", Label: "pvedata1"}},
	})
	// Guards that must never be dropped: no reformat of a disk that already has
	// a filesystem, and no duplicate fstab line on a re-run.
	for _, guard := range []string{`if ! blkid "$dev"`, "if ! grep -qF ' /data '"} {
		if !strings.Contains(got, guard) {
			t.Errorf("script is missing the idempotency guard %q:\n%s", guard, got)
		}
	}
}

// A pool whose disks are all fs=none must not touch SSH at all. The test would
// hang or error if setupGuestDisks tried to connect, since there is no VM.
func TestSetupGuestDisksNoopWithoutMountingDisks(t *testing.T) {
	d := &Driver{DiskSetupTimeout: time.Second}
	err := d.setupGuestDisks([]proxmox.AttachedDisk{
		{Device: "scsi1", Spec: proxmox.DiskSpec{Size: 10, Storage: "s", FS: proxmox.FSNone, Label: "pvedata1"}},
	})
	if err != nil {
		t.Fatalf("setupGuestDisks() with no mounting disks = %v, want nil", err)
	}
}

func TestSetupGuestDisksNoopWithNoDisks(t *testing.T) {
	d := &Driver{DiskSetupTimeout: time.Second}
	if err := d.setupGuestDisks(nil); err != nil {
		t.Fatalf("setupGuestDisks(nil) = %v, want nil", err)
	}
}

// The script is shipped base64-encoded so no quoting of the script body is
// needed on the remote command line.
func TestDiskSetupCommandIsBase64Encoded(t *testing.T) {
	cmd := diskSetupCommand("echo hi\n")
	if !strings.HasPrefix(cmd, "echo ") || !strings.Contains(cmd, "base64 -d | sudo bash -s") {
		t.Fatalf("diskSetupCommand() = %q, want an echo | base64 -d | sudo bash -s pipeline", cmd)
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(cmd, "echo "), " | base64 -d | sudo bash -s")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	if string(decoded) != "echo hi\n" {
		t.Errorf("decoded payload = %q, want %q", decoded, "echo hi\n")
	}
}
