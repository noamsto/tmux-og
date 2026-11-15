package main

import (
	"encoding/json"
	"regexp"
)

var prURLRe = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/[0-9]+$`)

func prURLWellformed(u string) bool { return prURLRe.MatchString(u) }

// sameRepoPR keeps the first PR that is not from a fork, as a one-element
// array without the isCrossRepository flag, or "[]".
func sameRepoPR(raw []byte) []byte {
	var prs []map[string]json.RawMessage
	if json.Unmarshal(raw, &prs) != nil {
		return []byte("[]")
	}
	for _, pr := range prs {
		if cross, ok := pr["isCrossRepository"]; ok && string(cross) != "null" && string(cross) != "false" {
			continue
		}
		delete(pr, "isCrossRepository")
		out, err := json.Marshal([]map[string]json.RawMessage{pr})
		if err != nil {
			return []byte("[]")
		}
		return out
	}
	return []byte("[]")
}
