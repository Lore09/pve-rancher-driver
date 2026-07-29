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

// forbiddenMounts are paths a data disk must never be mounted at. Mounting a
// freshly formatted, empty filesystem over any of these shadows the running
// system: the node either fails to boot or comes up missing the binaries,
// configuration or state it needs, and the failure looks like a Rancher problem
// rather than a disk one.
//
// /var is listed but /var/lib/longhorn is fine — only the exact path is
// refused, since shadowing all of /var takes out the logs, the container
// runtime state and the package database at once.
var forbiddenMounts = map[string]bool{
	"/":      true,
	"/bin":   true,
	"/boot":  true,
	"/dev":   true,
	"/etc":   true,
	"/home":  true,
	"/lib":   true,
	"/lib64": true,
	"/proc":  true,
	"/root":  true,
	"/run":   true,
	"/sbin":  true,
	"/sys":   true,
	"/usr":   true,
	"/var":   true,
}

// forbiddenMountTrees are subtrees that are off limits in their entirety: there
// is no legitimate reason to put a data disk anywhere inside them, and several
// are kernel-managed virtual filesystems where a mount would simply break.
var forbiddenMountTrees = []string{
	"/bin/", "/boot/", "/dev/", "/etc/", "/lib/", "/lib64/",
	"/proc/", "/sbin/", "/sys/", "/usr/",
}

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
		normalized, err := normalizeMount(spec.Mount)
		if err != nil {
			return spec, fmt.Errorf("pve: --pve-data-disk entry %q: %w", entry, err)
		}
		spec.Mount = normalized
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

// normalizeMount canonicalises a mount path and refuses the ones that would
// damage the guest.
//
// Normalising first matters for the duplicate check: "/data" and "/data/" are
// the same mount point, and comparing the raw strings would let both through.
func normalizeMount(path string) (string, error) {
	// Collapse repeated separators and strip the trailing one, so /data//sub/
	// and /data/sub compare equal. Root stays "/" and is rejected below.
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if path != "/" {
		path = strings.TrimSuffix(path, "/")
	}

	// The shell-safety charset permits dots, so ".." segments are reachable and
	// would let a path escape the directory it appears to name.
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return "", fmt.Errorf("mount %q must not contain '..' segments", path)
		}
	}

	if forbiddenMounts[path] {
		return "", fmt.Errorf("mount %q is a system directory; mounting a data disk there would shadow the running system (use a subdirectory such as %s/data)", path, strings.TrimSuffix(path, "/"))
	}
	for _, tree := range forbiddenMountTrees {
		if strings.HasPrefix(path, tree) {
			return "", fmt.Errorf("mount %q is inside the system directory %s, which a data disk must not occupy", path, strings.TrimSuffix(tree, "/"))
		}
	}
	return path, nil
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
			// Nesting one data disk inside another is legal in Linux but depends
			// on mount order: `mount -a` follows fstab order, so the parent can
			// end up mounted over the child, silently hiding it.
			for existing := range mounts {
				if strings.HasPrefix(s.Mount, existing+"/") {
					return fmt.Errorf("pve: --pve-data-disk mount %q is nested inside %q; mounting one data disk under another depends on mount order and can hide the inner one", s.Mount, existing)
				}
				if strings.HasPrefix(existing, s.Mount+"/") {
					return fmt.Errorf("pve: --pve-data-disk mount %q is nested inside %q; mounting one data disk under another depends on mount order and can hide the inner one", existing, s.Mount)
				}
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
