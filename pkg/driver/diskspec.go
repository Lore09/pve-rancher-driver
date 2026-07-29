package driver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/lore09/pve-rancher-driver/pkg/proxmox"
)

// maxDataDisks is the number of usable SCSI slots for data disks: PVE exposes
// scsi0..scsi30 and scsi0 is the boot disk.
const maxDataDisks = 30

// safeShellValue is the whitelist every value that reaches the guest setup
// script must satisfy. The script is assembled by string concatenation, so this
// is the boundary that stops a mount path from becoming a shell command.
var safeShellValue = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// dataDiskDevice matches the SCSI slots a data disk may occupy. scsi0 is
// excluded on purpose: that is the boot disk.
var dataDiskDevice = regexp.MustCompile(`^scsi([1-9]|[12][0-9]|30)$`)

// ParseDataDisks turns the raw --pve-data-disk entries into validated specs.
//
// Each entry is a comma-separated list of key=value pairs, e.g.
//
//	size=100,storage=local-lvm,fs=ext4,mount=/var/lib/longhorn
//
// It is pure: no I/O, no PVE calls. PreCreateCheck runs it so a misconfigured
// machine pool fails before a VM is cloned.
func ParseDataDisks(entries []string) ([]proxmox.DiskSpec, error) {
	specs := make([]proxmox.DiskSpec, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		spec, err := parseDataDisk(entry, len(specs)+1)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	if len(specs) > maxDataDisks {
		return nil, fmt.Errorf("pve: at most %d data disks are supported, got %d", maxDataDisks, len(specs))
	}
	if err := checkDataDiskCollisions(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// parseDataDisk parses one entry. position is the disk's 1-based index among
// the kept disks and only supplies the default label.
func parseDataDisk(entry string, position int) (proxmox.DiskSpec, error) {
	spec := proxmox.DiskSpec{
		FS:       "ext4",
		Discard:  "on",
		IOThread: "1",
		Backup:   "0",
		Label:    fmt.Sprintf("pvedata%d", position),
	}
	sizeSeen := false
	for _, pair := range strings.Split(entry, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return spec, fmt.Errorf("pve: --pve-data-disk entry %q: %q is not a key=value pair", entry, pair)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "size":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return spec, fmt.Errorf("pve: --pve-data-disk entry %q: size must be a positive number of GB, got %q", entry, value)
			}
			spec.Size = n
			sizeSeen = true
		case "storage":
			spec.Storage = value
		case "fs":
			spec.FS = strings.ToLower(value)
		case "mount":
			spec.Mount = value
		case "label":
			spec.Label = value
		case "device":
			spec.Device = value
		case "discard":
			spec.Discard = strings.ToLower(value)
		case "iothread":
			spec.IOThread = value
		case "backup":
			spec.Backup = value
		default:
			return spec, fmt.Errorf("pve: --pve-data-disk entry %q: unknown key %q", entry, key)
		}
	}

	if !sizeSeen {
		return spec, fmt.Errorf("pve: --pve-data-disk entry %q: size is required", entry)
	}
	if spec.Storage == "" {
		return spec, fmt.Errorf("pve: --pve-data-disk entry %q: storage is required", entry)
	}
	switch spec.FS {
	case "ext4", "xfs", proxmox.FSNone:
	default:
		return spec, fmt.Errorf("pve: --pve-data-disk entry %q: fs must be ext4, xfs or none, got %q", entry, spec.FS)
	}
	if spec.FS == proxmox.FSNone {
		if spec.Mount != "" {
			return spec, fmt.Errorf("pve: --pve-data-disk entry %q: mount is not allowed with fs=none", entry)
		}
	} else {
		if spec.Mount == "" {
			return spec, fmt.Errorf("pve: --pve-data-disk entry %q: mount is required unless fs=none", entry)
		}
		if !strings.HasPrefix(spec.Mount, "/") {
			return spec, fmt.Errorf("pve: --pve-data-disk entry %q: mount must be an absolute path, got %q", entry, spec.Mount)
		}
		if !safeShellValue.MatchString(spec.Mount) {
			return spec, fmt.Errorf("pve: --pve-data-disk entry %q: mount contains characters outside A-Za-z0-9._/-", entry)
		}
	}
	if !safeShellValue.MatchString(spec.Label) {
		return spec, fmt.Errorf("pve: --pve-data-disk entry %q: label contains characters outside A-Za-z0-9._/-", entry)
	}
	if spec.Device != "" && !dataDiskDevice.MatchString(spec.Device) {
		return spec, fmt.Errorf("pve: --pve-data-disk entry %q: device must be scsi1..scsi30, got %q", entry, spec.Device)
	}
	if spec.Discard != "on" && spec.Discard != "off" {
		return spec, fmt.Errorf("pve: --pve-data-disk entry %q: discard must be on or off, got %q", entry, spec.Discard)
	}
	if spec.IOThread != "0" && spec.IOThread != "1" {
		return spec, fmt.Errorf("pve: --pve-data-disk entry %q: iothread must be 0 or 1, got %q", entry, spec.IOThread)
	}
	if spec.Backup != "0" && spec.Backup != "1" {
		return spec, fmt.Errorf("pve: --pve-data-disk entry %q: backup must be 0 or 1, got %q", entry, spec.Backup)
	}
	return spec, nil
}

// checkDataDiskCollisions rejects the combinations that would make two disks
// fight over the same identity: the same mount point, the same label (and so
// the same serial and fstab key) or the same explicit slot.
func checkDataDiskCollisions(specs []proxmox.DiskSpec) error {
	mounts := map[string]bool{}
	labels := map[string]bool{}
	devices := map[string]bool{}
	for _, s := range specs {
		if s.Mount != "" {
			if mounts[s.Mount] {
				return fmt.Errorf("pve: --pve-data-disk has a duplicate mount path %q", s.Mount)
			}
			mounts[s.Mount] = true
		}
		if labels[s.Label] {
			return fmt.Errorf("pve: --pve-data-disk has a duplicate label %q", s.Label)
		}
		labels[s.Label] = true
		if s.Device != "" {
			if devices[s.Device] {
				return fmt.Errorf("pve: --pve-data-disk has a duplicate device %q", s.Device)
			}
			devices[s.Device] = true
		}
	}
	return nil
}
