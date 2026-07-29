package driver

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// minAllowedVMID is the lowest VMID Proxmox permits: 1-99 are reserved for
	// internal use and the UI refuses them.
	minAllowedVMID = 100
	// maxAllowedVMID is Proxmox's upper bound for a VMID.
	maxAllowedVMID = 999999999
)

// parseVMIDRange parses a "<min>-<max>" VMID range, e.g. "200-299".
//
// An empty string means "no range configured", which the caller treats as
// "let Proxmox pick with /cluster/nextid".
func parseVMIDRange(s string) (minID, maxID int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}

	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("pve: --pve-vmid-range %q must be written as <min>-<max>, e.g. 200-299", s)
	}

	minID, err = strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return 0, 0, fmt.Errorf("pve: --pve-vmid-range %q: %q is not a number", s, strings.TrimSpace(lo))
	}
	maxID, err = strconv.Atoi(strings.TrimSpace(hi))
	if err != nil {
		return 0, 0, fmt.Errorf("pve: --pve-vmid-range %q: %q is not a number", s, strings.TrimSpace(hi))
	}

	if minID < minAllowedVMID {
		return 0, 0, fmt.Errorf("pve: --pve-vmid-range %q: the lowest usable VMID is %d (1-99 are reserved by Proxmox)", s, minAllowedVMID)
	}
	if maxID > maxAllowedVMID {
		return 0, 0, fmt.Errorf("pve: --pve-vmid-range %q: the highest usable VMID is %d", s, maxAllowedVMID)
	}
	if maxID < minID {
		return 0, 0, fmt.Errorf("pve: --pve-vmid-range %q: the maximum (%d) is below the minimum (%d)", s, maxID, minID)
	}
	return minID, maxID, nil
}
