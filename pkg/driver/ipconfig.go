package driver

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
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

// staticPool is the range of addresses a machine pool may hand out, plus the
// netmask its machines get.
//
// Start and end are separate from the prefix on purpose. The prefix is the
// node's *netmask* — what it uses to decide whether an address is on-link —
// while start/end bound the pool. Folding the two into one CIDR made people
// write /28 to mean "16 addresses", which silently narrowed the subnet until
// the real gateway fell outside it and the node came up with no default route.
type staticPool struct {
	start netip.Addr
	end   netip.Addr
	bits  int
}

// prefix returns the subnet the machines believe they are on.
func (p staticPool) prefix() netip.Prefix {
	return netip.PrefixFrom(p.start, p.bits)
}

// capacity is how many machines the pool can address.
//
// This — not the width of the VMID range — is what caps a pool. VMIDs come
// from NextFreeVMID, which returns the *lowest* free id, so machines fill the
// pool from the start upward and it only ever has to hold the machines running
// at once.
func (p staticPool) capacity() int {
	s := p.start.As4()
	e := p.end.As4()
	return int(binary.BigEndian.Uint32(e[:])-binary.BigEndian.Uint32(s[:])) + 1
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

// parseStaticAddr parses one IPv4 address, naming the flag it came from.
func parseStaticAddr(flag, value string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("pve: %s %q must be an IPv4 address, e.g. 192.168.15.150", flag, value)
	}
	if !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("pve: %s %q must be IPv4; IPv6 addressing is not supported", flag, value)
	}
	return addr, nil
}

// parsePrefixBits parses --pve-ip-prefix, accepting either "24" or "/24".
func parsePrefixBits(value string) (int, error) {
	s := strings.TrimPrefix(strings.TrimSpace(value), "/")
	bits, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("pve: --pve-ip-prefix %q must be a number of bits, e.g. 24", value)
	}
	// Below /8 is never a node network, and above /30 leaves no usable hosts
	// once the network and broadcast addresses are excluded.
	if bits < 8 || bits > minStaticPrefixBits {
		return 0, fmt.Errorf("pve: --pve-ip-prefix /%d is out of range; use a value between /8 and /%d", bits, minStaticPrefixBits)
	}
	return bits, nil
}

// parseStaticPool parses and validates the three pool fields together.
//
// The prefix is deliberately its own field. It is the netmask the machines get,
// not the size of the pool: folding the two into one CIDR led people to write
// /28 meaning "16 addresses", which narrowed the subnet until the gateway fell
// outside it and the node booted with no usable default route.
func parseStaticPool(start, end, prefix string) (staticPool, error) {
	var p staticPool

	bits, err := parsePrefixBits(prefix)
	if err != nil {
		return p, err
	}
	first, err := parseStaticAddr("--pve-ip-start", start)
	if err != nil {
		return p, err
	}
	last, err := parseStaticAddr("--pve-ip-end", end)
	if err != nil {
		return p, err
	}
	p = staticPool{start: first, end: last, bits: bits}

	if last.Less(first) {
		return staticPool{}, fmt.Errorf("pve: --pve-ip-end %s is below --pve-ip-start %s", last, first)
	}
	// Both ends must sit in the same subnet, or the machines at one end would get
	// a netmask that does not describe the network they are actually on.
	subnet := p.prefix().Masked()
	if !subnet.Contains(last) {
		return staticPool{}, fmt.Errorf("pve: --pve-ip-start %s and --pve-ip-end %s are not in the same /%d subnet (%s); widen --pve-ip-prefix or move one end", first, last, bits, subnet)
	}
	network, broadcast, err := networkAndBroadcast(p.prefix())
	if err != nil {
		return staticPool{}, err
	}
	if first == network || last == network {
		return staticPool{}, fmt.Errorf("pve: the pool includes %s, the network address of %s, which cannot be assigned to a machine", network, subnet)
	}
	if first == broadcast || last == broadcast {
		return staticPool{}, fmt.Errorf("pve: the pool includes %s, the broadcast address of %s, which cannot be assigned to a machine", broadcast, subnet)
	}
	return p, nil
}

// buildIPConfig renders the cloud-init ipconfig0 value for one machine.
//
// The static address is derived from the VMID rather than allocated, because
// the driver is a stateless per-machine process with nothing to allocate
// against. offset = vmid - vmidMin, so machines fill the pool from its start
// upward with no coordination and no races.
//
// IMPORTANT: callers must pass the *final* VMID. cloneFromTemplate retries when
// it loses a VMID claim, so an address computed before the claim settles would
// belong to a VMID this machine did not get.
func buildIPConfig(mode ipMode, start, end, prefix, gateway string, vmid, vmidMin int) (string, error) {
	if mode != ipModeStatic {
		return "ip=dhcp", nil
	}
	pool, err := parseStaticPool(start, end, prefix)
	if err != nil {
		return "", err
	}
	offset := vmid - vmidMin
	if offset < 0 {
		return "", fmt.Errorf("pve: VMID %d is below the --pve-vmid-range minimum %d, so it has no address in the pool", vmid, vmidMin)
	}
	// The pool, not the VMID range, is what bounds the machine count. Report
	// exhaustion in those terms so the number is actionable.
	if capacity := pool.capacity(); offset >= capacity {
		return "", fmt.Errorf("pve: static IP pool exhausted: %s-%s holds %d machines, and VMID %d needs slot %d; widen the pool or scale down", pool.start, pool.end, capacity, vmid, offset+1)
	}
	addr, err := offsetAddr(pool.start, offset)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ip=%s/%d,gw=%s", addr, pool.bits, strings.TrimSpace(gateway)), nil
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
	return buildIPConfig(mode, d.IPStart, d.IPEnd, d.IPPrefix, d.Gateway, d.VMID, vmidMin)
}

// validateStaticPool checks the pool and gateway at pool-save time.
//
// It deliberately does NOT require the pool to be as large as the VMID range.
// NextFreeVMID hands out the lowest free id, so machines fill the pool from its
// start upward and it only has to hold the machines running at once. Requiring
// full coverage forced absurd pool sizes — a 100-wide VMID range demanded 100
// addresses even to run three nodes. Exhaustion is reported per machine by
// buildIPConfig instead, with the pool size named.
func validateStaticPool(start, end, prefix, gateway string, vmidMin, vmidMax int) error {
	pool, err := parseStaticPool(start, end, prefix)
	if err != nil {
		return err
	}
	gw, err := parseStaticAddr("--pve-gateway", gateway)
	if err != nil {
		return err
	}
	// The gateway belongs to the subnet, not the pool: it is normal and expected
	// for it to sit outside the pool's range. But a gateway outside the *subnet*
	// is not reachable on-link, so the node would boot unable to install a
	// default route.
	subnet := pool.prefix().Masked()
	if !subnet.Contains(gw) {
		return fmt.Errorf("pve: --pve-gateway %s is outside %s, the subnet the machines get from --pve-ip-prefix /%d. The gateway may sit outside the pool %s-%s, but it must be inside the subnet or the node has no on-link route to it — widen --pve-ip-prefix to match the real network", gw, subnet, pool.bits, pool.start, pool.end)
	}
	// Not an error: a VMID range wider than the pool is a normal way to keep id
	// headroom. Surface the real capacity so it is not a surprise later.
	if capacity, size := pool.capacity(), vmidMax-vmidMin+1; size > capacity {
		log.Infof("pve: --pve-vmid-range %d-%d allows %d machines but the pool %s-%s holds %d; the pool caps this machine pool at %d", vmidMin, vmidMax, size, pool.start, pool.end, capacity, capacity)
	}
	return nil
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
		// Rejected rather than ignored: leftover pool fields from a static config
		// look like they are in effect, and the node then comes up on a DHCP
		// address nobody expected.
		var orphans []string
		for flag, value := range map[string]string{
			"--pve-ip-start":  d.IPStart,
			"--pve-ip-end":    d.IPEnd,
			"--pve-ip-prefix": d.IPPrefix,
			"--pve-gateway":   d.Gateway,
		} {
			if value != "" {
				orphans = append(orphans, flag)
			}
		}
		if len(orphans) > 0 {
			sort.Strings(orphans) // map iteration order is random; keep the message stable
			return fmt.Errorf("pve: %s only apply when --pve-ip-mode is static", strings.Join(orphans, ", "))
		}
		return nil
	}

	// Ordered, not a map: the form reports the first missing field, and it should
	// be the same one the driver names.
	for _, f := range []struct{ flag, value string }{
		{"--pve-ip-start", d.IPStart},
		{"--pve-ip-end", d.IPEnd},
		{"--pve-ip-prefix", d.IPPrefix},
		{"--pve-gateway", d.Gateway},
	} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("pve: %s is required when --pve-ip-mode is static", f.flag)
		}
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
	return validateStaticPool(d.IPStart, d.IPEnd, d.IPPrefix, d.Gateway, vmidMin, vmidMax)
}
