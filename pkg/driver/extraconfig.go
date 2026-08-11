package driver

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/lore09/pve-rancher-driver/pkg/proxmox"
)

// extraConfigKeyPattern is the shape of a PVE VM config key: lowercase, and
// possibly ending in an index (hostpci0, tpmstate0). Validating it here turns a
// typo into a message naming the flag rather than a 400 from the PVE API
// halfway through Create, with the VM already cloned.
var extraConfigKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ParseExtraConfig turns the --pve-extra-config occurrences into ordered PVE
// config options.
//
// One occurrence is one option, split on its *first* `=` only: PVE config
// values are themselves comma- and equals-separated lists (`startup=order=1,up=30`,
// `hostpci0=0000:01:00,pcie=1`), so anything cleverer would have to re-implement
// PVE's own property-string grammar to hand it straight back.
//
// reserved maps a key the driver writes itself to the flag that owns it. Those
// are rejected rather than overwritten: the driver reads several of them back
// (the NIC's MAC pins IP discovery, the boot disk device is what gets resized)
// and a silent override would surface much later as a node that never reports
// an address.
func ParseExtraConfig(entries []string, reserved map[string]string) ([]proxmox.ConfigOption, error) {
	opts := make([]proxmox.ConfigOption, 0, len(entries))
	seen := make(map[string]bool, len(entries))

	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		idx := strings.Index(entry, "=")
		if idx <= 0 {
			return nil, fmt.Errorf("pve: --pve-extra-config %q is not a key=value pair, e.g. cpu=host", entry)
		}
		key := strings.ToLower(strings.TrimSpace(entry[:idx]))
		value := strings.TrimSpace(entry[idx+1:])

		if !extraConfigKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("pve: --pve-extra-config key %q is not a valid PVE config key; use lowercase letters, digits and underscores, e.g. cpu, numa, hostpci0", key)
		}
		if value == "" {
			return nil, fmt.Errorf("pve: --pve-extra-config %q has an empty value; PVE has no way to express \"unset\" here, so drop the entry instead", key)
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("pve: --pve-extra-config %q value must be a single line", key)
		}
		if owner, taken := reserved[key]; taken {
			return nil, fmt.Errorf("pve: --pve-extra-config cannot set %q; the driver writes that key itself from %s, and overriding it here would be silently undone or would break the driver's own use of it", key, owner)
		}
		if seen[key] {
			return nil, fmt.Errorf("pve: --pve-extra-config sets %q more than once; PVE keeps one value per key, so only the last would apply", key)
		}
		seen[key] = true

		opts = append(opts, proxmox.ConfigOption{Key: key, Value: value})
	}

	return opts, nil
}

// ciCustomVolumePattern matches the `storage:snippets/file` volume id PVE
// expects. The `snippets/` segment is not a convention: cicustom only accepts a
// volume on a storage with the `snippets` content type enabled, so a path
// without it fails at create time.
var ciCustomVolumePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+:snippets/[A-Za-z0-9._/-]+$`)

// ciCustomTypes are the four cloud-init parts PVE can source from a snippet.
var ciCustomTypes = map[string]bool{"meta": true, "network": true, "user": true, "vendor": true}

// validateCICustom checks --pve-cicustom before anything is cloned.
//
// staticIP reports whether the driver is generating ipconfig0 itself, which a
// `network=` snippet would replace.
func validateCICustom(raw string, staticIP bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		typ, volume, ok := strings.Cut(part, "=")
		typ = strings.ToLower(strings.TrimSpace(typ))
		volume = strings.TrimSpace(volume)
		if !ok || volume == "" {
			return fmt.Errorf("pve: --pve-cicustom %q is not a <type>=<volume> pair, e.g. vendor=local:snippets/rancher.yaml", part)
		}
		if !ciCustomTypes[typ] {
			return fmt.Errorf("pve: --pve-cicustom type %q is not one of %s", typ, strings.Join(sortedCICustomTypes(), ", "))
		}
		if seen[typ] {
			return fmt.Errorf("pve: --pve-cicustom sets %q twice; PVE takes one snippet per type", typ)
		}
		seen[typ] = true

		if !ciCustomVolumePattern.MatchString(volume) {
			return fmt.Errorf("pve: --pve-cicustom volume %q must be <storage>:snippets/<file>, e.g. local:snippets/rancher.yaml — cicustom only reads from a storage with the `snippets` content type enabled", volume)
		}
	}

	// PVE generates user-data *or* takes yours, never both: a `user=` snippet
	// replaces the generated one wholesale, and the generated one is what
	// carries ciuser and sshkeys. Those keys are minted per machine at create
	// time, so a snippet written in advance cannot contain them — the node
	// would come up with nobody able to log in, and the driver could not even
	// format its data disks. `vendor=` is the channel that merges instead.
	if seen["user"] {
		return fmt.Errorf("pve: --pve-cicustom user=... replaces the cloud-init user-data PVE generates, which is the only thing carrying the SSH key the driver and Rancher log in with — and that key is generated per machine, so no pre-written snippet can contain it. Use vendor=... instead: PVE generates no vendor-data, so yours is merged rather than substituted")
	}

	// ipconfig0 and a network snippet are the same slot. In DHCP mode the
	// driver writes nothing worth protecting, so a network snippet is a
	// legitimate way to configure something ipconfig0 cannot express.
	if seen["network"] && staticIP {
		return fmt.Errorf("pve: --pve-cicustom network=... replaces the network config PVE renders from ipconfig0, which is how --pve-ip-mode=static assigns each machine its address; the two cannot both apply. Drop the network snippet, or switch --pve-ip-mode to dhcp and let the snippet own addressing")
	}

	return nil
}

func sortedCICustomTypes() []string {
	out := make([]string, 0, len(ciCustomTypes))
	for t := range ciCustomTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
