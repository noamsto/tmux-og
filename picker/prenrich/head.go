package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// headGraceSeconds is how long a head, once seen, may still be registering its
// checks: a rollup of only settled entries is not trusted as success before it.
const headGraceSeconds = 120

type identityPR struct{ state, head, createdAt string }

func readIdentity(cache string) identityPR {
	b, err := os.ReadFile(cache) //nolint:gosec // cache path is built from the poller's own dir and a sha1 key
	if err != nil {
		return identityPR{}
	}
	var prs []struct{ State, HeadRefOid, CreatedAt string }
	if json.Unmarshal(b, &prs) != nil || len(prs) == 0 {
		return identityPR{}
	}
	return identityPR{prs[0].State, prs[0].HeadRefOid, prs[0].CreatedAt}
}

// stampPath is the per-branch "<oid>\n<first-seen epoch>" file. Its mtime marks
// when the poller first saw that head.
func stampPath(cache string) string {
	return strings.TrimSuffix(cache, ".json") + ".head-seen"
}

// headStale reports that the checks cache cannot be trusted for the open PR's
// live head. A differing head is always stale. An empty checks head (no cache,
// or the PR was outside the checks batch) is stale only while the checks cache
// predates our sight of the head, so a PR the batch never lists is refetched
// once per head rather than every pass.
func headStale(cache string) bool {
	id := readIdentity(cache)
	if id.state != "OPEN" || id.head == "" {
		return false
	}
	checks := checksPath(cache)
	if h := headRefOid(checks); h != "" {
		return h != id.head
	}
	seen, fetched := fileMtime(stampPath(cache)), fileMtime(checks)
	return seen == 0 || fetched == 0 || fetched < seen
}

// headSettling reports whether the open PR's head was first seen less than
// headGraceSeconds ago, recording the sighting when the head is new. A head
// with no stamp counts from the PR's creation, so an old PR after a cache wipe
// gets no grace.
func (e *enricher) headSettling(cache, head, createdAt string) bool {
	if head == "" {
		return false
	}
	stamp := stampPath(cache)
	now := e.now().Unix()
	oid, firstSeen := readStamp(stamp)
	if oid != head {
		firstSeen = now
		if oid == "" {
			firstSeen = createdEpoch(createdAt)
		}
		if writeAtomic(stamp, []byte(head+"\n"+strconv.FormatInt(firstSeen, 10)+"\n")) == nil {
			// Dated by the identity fetch that showed the head, not by this
			// later write: a checks refetch landing in between is not stale.
			touchAt(stamp, time.Unix(fileMtime(cache), 0))
		}
	}
	return now-firstSeen < headGraceSeconds
}

func readStamp(path string) (oid string, firstSeen int64) {
	b, err := os.ReadFile(path) //nolint:gosec // stamp path is the poller's own cache dir plus a sha1 key
	if err != nil {
		return "", 0
	}
	oid, rest, _ := strings.Cut(string(b), "\n")
	line, _, _ := strings.Cut(rest, "\n")
	firstSeen, err = strconv.ParseInt(line, 10, 64)
	if err != nil {
		return "", 0
	}
	return oid, firstSeen
}

func createdEpoch(createdAt string) int64 {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// episodeHead identifies a pending episode: the live head when the identity
// cache carries it, else the head stored with the rollup.
func episodeHead(cache string) string {
	if h := headRefOid(cache); h != "" {
		return h
	}
	return headRefOid(checksPath(cache))
}
