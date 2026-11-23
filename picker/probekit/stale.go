package probekit

import "time"

// RemoteCacheStaleAfter is how old a cached session list may get before its
// rows render dimmed with their age.
const RemoteCacheStaleAfter = 5 * time.Minute

func SavedAtStale(savedAt int64, now time.Time) bool {
	return now.Sub(time.UnixMilli(savedAt)) > RemoteCacheStaleAfter
}
