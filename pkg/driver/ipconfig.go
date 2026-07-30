package driver

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/docker/machine/libmachine/log"
)

// ipMode selects how a machine gets its address.
type ipMode string

const (
	ipModeDHCP   ipMode = "dhcp"
	ipModeStatic ipMode = "static"
)

// parseIPMode parses the --pve-ip-mode value. An empty string means dhcp, so
// that a pool created before this flag existed keeps its previous behaviour.
func parseIPMode(s string) (ipMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(ipModeDHCP):
		return ipModeDHCP, nil
	case string(ipModeStatic):
		return ipModeStatic, nil
	default:
		return "", fmt.Errorf("pve: --pve-ip-mode %q is not valid; use dhcp or static", s)
	}
}

// normalizeNameservers accepts a comma- or space-separated list and returns
// PVE's space-separated form.
//
// Each entry is parsed as an IP because PVE stores the `nameserver` string
// without validating it: a hostname or a typo is accepted by the API and only
// shows up later as a node that cannot resolve anything.
func normalizeNameservers(s string) (string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(fields) == 0 {
		return "", nil
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		addr, err := netip.ParseAddr(f)
		if err != nil {
			return "", fmt.Errorf("pve: --pve-nameservers entry %q is not a valid IP address", f)
		}
		out = append(out, addr.String())
	}
	return strings.Join(out, " "), nil
}

// minStaticPrefixBits is the smallest prefix that leaves usable host
// addresses once the network and broadcast addresses are excluded. A /31 or
// /32 has none, so it can only ever be a misconfiguration here.
const minStaticPrefixBits = 30

// offsetAddr returns addr advanced by n addresses. IPv4 only.
func offsetAddr(addr netip.Addr, n int) (netip.Addr, error) {
	if !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("pve: %s is not an IPv4 address", addr)
	}
	if n < 0 {
		return netip.Addr{}, fmt.Errorf("pve: cannot offset %s by a negative amount (%d)", addr, n)
	}
	b := addr.As4()
	sum := uint64(binary.BigEndian.Uint32(b[:])) + uint64(n)
	if sum > 0xFFFFFFFF {
		return netip.Addr{}, fmt.Errorf("pve: %s + %d runs past the end of the IPv4 address space", addr, n)
	}
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], uint32(sum))
	return netip.AddrFrom4(out), nil
}

// capacityFrom returns how many machines can be addressed starting at the base,
// counting up to but not including the subnet's broadcast address.
//
// This — not the width of the VMID range — is what caps a static pool. VMIDs
// come from NextFreeVMID, which returns the *lowest* free id in the range, so
// machines cluster at the bottom of the range and the subnet only ever has to
// hold the machines that exist at once. Sizing the subnet to the whole VMID
// range instead demanded a /25 to run three nodes with a 100-wide range.
func capacityFrom(pfx netip.Prefix) (int, error) {
	_, broadcast, err := networkAndBroadcast(pfx)
	if err != nil {
		return 0, err
	}
	first := pfx.Addr().As4()
	last := broadcast.As4()
	return int(binary.BigEndian.Uint32(last[:]) - binary.BigEndian.Uint32(first[:])), nil
}

// networkAndBroadcast returns the two addresses in pfx that must never be
// assigned to a machine.
func networkAndBroadcast(pfx netip.Prefix) (netip.Addr, netip.Addr, error) {
	masked := pfx.Masked()
	network := masked.Addr()
	hostCount := uint64(1)<<(32-uint(masked.Bits())) - 1
	broadcast, err := offsetAddr(network, int(hostCount))
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	return network, broadcast, nil
}

// parseIPBase parses --pve-ip-base and rejects everything this design does not
// support, with a message naming the flag.
func parseIPBase(base string) (netip.Prefix, error) {
	pfx, err := netip.ParsePrefix(strings.TrimSpace(base))
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("pve: --pve-ip-base %q must be an IPv4 address with a prefix, e.g. 10.10.20.10/24", base)
	}
	if !pfx.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("pve: --pve-ip-base %q must be IPv4; IPv6 addressing is not supported", base)
	}
	if pfx.Bits() > minStaticPrefixBits {
		return netip.Prefix{}, fmt.Errorf("pve: --pve-ip-base %q has no usable host addresses; use /30 or larger", base)
	}
	return pfx, nil
}

// buildIPConfig renders the cloud-init ipconfig0 value for one machine.
//
// The static address is derived from the VMID rather than allocated, because
// the driver is a stateless per-machine process with nothing to allocate
// against. offset = vmid - vmidMin, so the VMID range and the address range
// stay in lockstep with no coordination and no races.
//
// IMPORTANT: callers must pass the *final* VMID. cloneFromTemplate retries when
// it loses a VMID claim, so an address computed before the claim settles would
// belong to a VMID this machine did not get.
func buildIPConfig(mode ipMode, base, gateway string, vmid, vmidMin int) (string, error) {
	if mode != ipModeStatic {
		return "ip=dhcp", nil
	}
	pfx, err := parseIPBase(base)
	if err != nil {
		return "", err
	}
	offset := vmid - vmidMin
	if offset < 0 {
		return "", fmt.Errorf("pve: VMID %d is below the --pve-vmid-range minimum %d, so it has no address in the pool", vmid, vmidMin)
	}
	addr, err := offsetAddr(pfx.Addr(), offset)
	if err != nil {
		return "", err
	}
	// The subnet, not the VMID range, is what bounds the pool. Report exhaustion
	// in those terms: the operator needs to know how many machines the subnet
	// can hold, not that some arithmetic left the prefix.
	capacity, err := capacityFrom(pfx)
	if err != nil {
		return "", err
	}
	if offset >= capacity {
		return "", fmt.Errorf("pve: static IP pool exhausted: --pve-ip-base %s leaves room for %d machines from %s, and VMID %d needs offset %d; widen the subnet, lower the base address, or scale the pool down", base, capacity, pfx.Addr(), vmid, offset)
	}
	if !pfx.Contains(addr) {
		return "", fmt.Errorf("pve: VMID %d maps to %s, which is outside the subnet of --pve-ip-base %s", vmid, addr, base)
	}
	network, broadcast, err := networkAndBroadcast(pfx)
	if err != nil {
		return "", err
	}
	if addr == network {
		return "", fmt.Errorf("pve: VMID %d maps to %s, which is the network address of %s", vmid, addr, base)
	}
	if addr == broadcast {
		return "", fmt.Errorf("pve: VMID %d maps to %s, which is the broadcast address of %s", vmid, addr, base)
	}
	return fmt.Sprintf("ip=%s/%d,gw=%s", addr, pfx.Bits(), strings.TrimSpace(gateway)), nil
}

// validateStaticSpan checks the static addressing config at pool-save time.
//
// It deliberately does NOT require the subnet to cover the whole VMID range.
// NextFreeVMID hands out the lowest free id in the range, so machines cluster
// at the bottom of it and the subnet only ever has to hold the machines that
// exist at once. Requiring full coverage forced absurd subnet sizes — a
// 100-wide VMID range demanded a /25 even to run three nodes.
//
// What it does check is that the base address is usable at all, so the errors a
// user can only hit by scaling are left to buildIPConfig, which reports them as
// pool exhaustion with a machine count.
func validateStaticSpan(base, gateway string, vmidMin, vmidMax int) error {
	pfx, err := parseIPBase(base)
	if err != nil {
		return err
	}
	gw, err := netip.ParseAddr(strings.TrimSpace(gateway))
	if err != nil || !gw.Is4() {
		return fmt.Errorf("pve: --pve-gateway %q must be an IPv4 address", gateway)
	}
	if !pfx.Contains(gw) {
		return fmt.Errorf("pve: --pve-gateway %s is outside the subnet of --pve-ip-base %s; a gateway the nodes cannot reach is almost always a typo", gateway, base)
	}
	network, broadcast, err := networkAndBroadcast(pfx)
	if err != nil {
		return err
	}
	if pfx.Addr() == network {
		return fmt.Errorf("pve: --pve-ip-base %s is the network address of its subnet, which cannot be assigned to a machine; use the next address", base)
	}
	if pfx.Addr() == broadcast {
		return fmt.Errorf("pve: --pve-ip-base %s is the broadcast address of its subnet, which cannot be assigned to a machine", base)
	}
	// The base must leave room for at least one machine. Anything beyond that is
	// a capacity question, reported per machine by buildIPConfig.
	capacity, err := capacityFrom(pfx)
	if err != nil {
		return err
	}
	if capacity < 1 {
		return fmt.Errorf("pve: --pve-ip-base %s leaves no room for any machine before the end of its subnet; lower the base address or widen the subnet", base)
	}
	// Not an error: a VMID range wider than the subnet is a normal way to keep
	// id headroom. Surface the real capacity so it is not a surprise later.
	if size := vmidMax - vmidMin + 1; size > capacity {
		log.Infof("pve: --pve-vmid-range %d-%d allows %d machines but --pve-ip-base %s addresses %d; the pool is capped at %d machines by the subnet", vmidMin, vmidMax, size, base, capacity, capacity)
	}
	return nil
}

// resolveIPConfig renders the ipconfig0 value for this machine.
//
// Call this only after d.VMID holds the id the clone actually claimed.
func (d *Driver) resolveIPConfig() (string, error) {
	mode, err := parseIPMode(d.IPMode)
	if err != nil {
		return "", err
	}
	if mode != ipModeStatic {
		return "ip=dhcp", nil
	}
	// Guarded here as well as in validateAddressing: parseVMIDRange("") returns
	// (0, 0, nil), which would silently make the offset vmid-0 and hand out an
	// address nowhere near the pool's base.
	if strings.TrimSpace(d.VMIDRange) == "" {
		return "", errors.New("pve: --pve-ip-mode static requires --pve-vmid-range: each machine's address is derived from its position in that range")
	}
	vmidMin, _, err := parseVMIDRange(d.VMIDRange)
	if err != nil {
		return "", err
	}
	return buildIPConfig(mode, d.IPBase, d.Gateway, d.VMID, vmidMin)
}

// validateAddressing checks every addressing and DNS field. It runs in
// PreCreateCheck because failing there is free: failing after the clone means a
// half-built VM to roll back.
//
// It also normalises d.Nameservers in place, so Configure can use the value
// without re-parsing it.
func (d *Driver) validateAddressing() error {
	mode, err := parseIPMode(d.IPMode)
	if err != nil {
		return err
	}

	// DNS reaches the guest through cloud-init, so with cloud-init off PVE
	// would never be sent these at all.
	if !d.CloudInit && (d.Nameservers != "" || d.SearchDomain != "") {
		return errors.New("pve: --pve-nameservers and --pve-searchdomain need --pve-cloudinit: PVE applies them as cloud-init options, so without it they would be silently dropped")
	}
	ns, err := normalizeNameservers(d.Nameservers)
	if err != nil {
		return err
	}
	d.Nameservers = ns

	if mode == ipModeDHCP {
		// Rejected rather than ignored: a leftover base from a static config
		// looks like it is in effect, and the node then comes up on a DHCP
		// address nobody expected.
		var orphans []string
		if d.IPBase != "" {
			orphans = append(orphans, "--pve-ip-base")
		}
		if d.Gateway != "" {
			orphans = append(orphans, "--pve-gateway")
		}
		if len(orphans) > 0 {
			return fmt.Errorf("pve: %s only apply when --pve-ip-mode is static", strings.Join(orphans, ", "))
		}
		return nil
	}

	if d.IPBase == "" {
		return errors.New("pve: --pve-ip-base is required when --pve-ip-mode is static")
	}
	if d.Gateway == "" {
		return errors.New("pve: --pve-gateway is required when --pve-ip-mode is static")
	}
	if !d.CloudInit {
		return errors.New("pve: --pve-ip-mode static needs --pve-cloudinit: the address is delivered through cloud-init ipconfig0")
	}
	// The address is base + (vmid - rangeMin), so without a range the VMID
	// comes from /cluster/nextid, is unbounded, and the offset is meaningless.
	if d.VMIDRange == "" {
		return errors.New("pve: --pve-ip-mode static requires --pve-vmid-range: each machine's address is derived from its position in that range")
	}
	vmidMin, vmidMax, err := parseVMIDRange(d.VMIDRange)
	if err != nil {
		return err
	}
	return validateStaticSpan(d.IPBase, d.Gateway, vmidMin, vmidMax)
}
