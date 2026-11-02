//go:build race

package linkjoin

import "time"

// The race detector slows the regexp engine about fifteenfold; the budget
// still fails a quadratic rejoin by orders of magnitude.
func init() { blobBudget = 10 * time.Second }
