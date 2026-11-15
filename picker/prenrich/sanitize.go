package main

import "strings"

var titleStrip = strings.NewReplacer("\r", "", "\n", "", "\x1b", "", "'", "", "#", "", "|", " ")

// sanitizeTitle strips what would break a tmux format or the |-delimited
// readers, then truncates to GitHub's 256-character title limit.
func sanitizeTitle(raw string) string {
	r := []rune(titleStrip.Replace(raw))
	if len(r) > 256 {
		r = r[:256]
	}
	return string(r)
}
