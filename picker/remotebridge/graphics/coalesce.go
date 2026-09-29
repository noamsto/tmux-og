package graphics

import "slices"

// Coalesce drops stores that a later store in the same batch supersedes.
//
// This is safe because of how a viewer pans: each store re-places under the same
// id with a=T and no delete, so a store is a full replacement and the
// intermediate frames are pure waste — fetching them would make the image trail
// the cursor. Dropping them lets the LINK set the frame rate, with the newest
// framing always the survivor.
//
// A delete for an id ends the supersede chain: it is a real state transition,
// not a stale frame, so the store before it must still be forwarded.
//
// "Batch" is whatever one Scanner.Feed call returns, not the whole stream. In
// the daemon's wiring that's exactly one pump iteration's drained burst
// (drainOutput's whole queued backlog, handed to Filter in one call), so two
// stores queued at the same moment do land in one batch together. Two stores
// separated in time instead — the second arriving only after the pump has
// already drained and gone back to waiting — fall into separate pump
// iterations and so separate batches: both get fetched. That's a missed
// optimisation, not a correctness problem, since the older one is still
// superseded once the newer one lands.
func Coalesce(in []Chunk) []Chunk {
	superseded := make([]bool, len(in))
	seen := map[string]bool{} // image id -> a later store exists
	for i, c := range slices.Backward(in) {
		q := c.Seq
		if q == nil {
			continue
		}
		id := q.Get("i")
		switch {
		case isDelete(q):
			delete(seen, id)
		case isStore(q):
			if seen[id] {
				superseded[i] = true
				continue
			}
			seen[id] = true
		}
	}
	out := make([]Chunk, 0, len(in))
	for i, c := range in {
		if !superseded[i] {
			out = append(out, c)
		}
	}
	return out
}
