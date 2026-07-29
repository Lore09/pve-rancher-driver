package proxmox

// FSNone is the DiskSpec.FS value that means "attach the disk and leave the
// guest completely alone". Used for raw block devices handed to something that
// wants them unformatted.
const FSNone = "none"

// DiskSpec describes one data disk to attach to a cloned VM.
//
// Label doubles as the PVE disk serial, which is what makes the disk findable
// inside the guest: kernel names (sdb, sdc, ...) are assigned in discovery
// order and cannot be relied on once a VM has more than one data disk.
type DiskSpec struct {
	Size     int    // size in GB, always > 0
	Storage  string // PVE storage id, e.g. "local-lvm"
	FS       string // "ext4", "xfs" or FSNone
	Mount    string // absolute mount path; empty when FS == FSNone
	Label    string // filesystem label, fstab key and PVE disk serial
	Device   string // PVE config key, e.g. "scsi3"; empty means "allocate one"
	Discard  string // "on" or "off"
	IOThread string // "0" or "1"
	Backup   string // "0" or "1"
}

// NeedsGuestSetup reports whether the driver has to format and mount this disk
// inside the guest.
func (s DiskSpec) NeedsGuestSetup() bool {
	return s.FS != "" && s.FS != FSNone
}

// AttachedDisk pairs a spec with the PVE config key the disk actually landed
// on, which is only known after slot allocation.
type AttachedDisk struct {
	Device string
	Spec   DiskSpec
}
