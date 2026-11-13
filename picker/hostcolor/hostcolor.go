// Package hostcolor assigns each configured remote host a palette slot. The
// session picker and the status line both use it so one host is one colour.
package hostcolor

import (
	"hash/fnv"
	"sort"
	"strings"
)

// Palette names the @thm_* tokens a host may be tinted with, in slot order.
// Excludes mauve, red and blue — local sessions, error states and the path
// icon already mean those.
var Palette = [5]string{"peach", "teal", "pink", "yellow", "sky"}

// ParseHosts splits a whitespace-separated @remote_bridge_hosts value into
// ssh Host aliases, dropping duplicates and keeping first-seen order.
func ParseHosts(raw string) []string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, h := range fields {
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

func preferredSlot(host string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(host))
	return int(h.Sum32() % uint32(len(Palette))) //nolint:gosec // G115: value is non-negative and fits uint32
}

// Name returns the palette name for host. Configured hosts are assigned in
// sorted order: each host's hash picks a preferred slot, then linear-probes to
// the next free one, so the set gets distinct colours when possible. More
// hosts than slots wrap and reuse. A host outside the list, or an empty list,
// falls back to the plain hash slot; an empty host yields "".
func Name(hosts []string, host string) string {
	if host == "" {
		return ""
	}
	n := len(Palette)
	sorted := append([]string(nil), hosts...)
	sort.Strings(sorted)
	assigned := make(map[string]int, len(sorted))
	used := make([]bool, n)
	for _, h := range sorted {
		preferred := preferredSlot(h)
		slot := preferred
		for i := range n {
			candidate := (preferred + i) % n
			slot = candidate
			if !used[candidate] {
				used[candidate] = true
				break
			}
		}
		assigned[h] = slot
	}
	if slot, ok := assigned[host]; ok {
		return Palette[slot]
	}
	return Palette[preferredSlot(host)]
}
