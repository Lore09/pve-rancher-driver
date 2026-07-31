package driver

import (
	"fmt"
	"regexp"
	"strings"
)

// pveTagPattern matches a single PVE tag. PVE itself lowercases and
// restricts tags to this character set; validating here catches a typo
// before the VM is cloned rather than after.
var pveTagPattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9_+.-]*$`)

// tagSeparator is what PVE stores between tags in the `tags` VM config
// value (go-proxmox calls the same constant TagSeperator internally).
const tagSeparator = ";"

// normalizeTags parses --pve-tags (comma-separated) into the lowercase,
// semicolon-separated form PVE's `tags` config option expects. Duplicates
// are dropped rather than rejected, since the same tag appearing twice
// (possibly differing only in case) is a harmless typo, not a config error
// worth failing PreCreateCheck over.
func normalizeTags(s string) (string, error) {
	fields := strings.Split(s, ",")
	tags := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		tag := strings.ToLower(strings.TrimSpace(f))
		if tag == "" {
			continue
		}
		if !pveTagPattern.MatchString(tag) {
			return "", fmt.Errorf("pve: --pve-tags %q is not a valid PVE tag; use lowercase letters, digits, and _ + . -, starting with a letter, digit or underscore", tag)
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	return strings.Join(tags, tagSeparator), nil
}
