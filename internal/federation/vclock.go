package federation

import "fmt"

// MaxVClockEntries caps the number of distinct cortex IDs in a merged
// vector clock. A legitimate federation ring rarely exceeds a handful of
// peers; a clock with hundreds of entries is a strong signal of a clock
// inflation attack (a malicious peer injecting synthetic cortex IDs into
// its event payloads). The cap prevents unbounded growth of the
// federation_state row that is serialized on every mutation.
const MaxVClockEntries = 256

// VClock is a vector clock: one counter per known peer.
type VClock map[string]uint64

// CortexIDKey reports whether key is shaped like a stable cortex identity.
// Cortex IDs are ULIDs, represented as 26-character strings. Older Noema
// releases used display names as vector-clock keys; those legacy buckets must
// not influence current federation causality.
func CortexIDKey(key string) bool {
	return len(key) == 26
}

// KeepCortexIDKeys returns a copy of vc containing only stable cortex-ID
// buckets. It is used on production federation paths to stop pre-migration
// name-keyed buckets from surviving forever in new event snapshots.
func KeepCortexIDKeys(vc VClock) VClock {
	clean := make(VClock, len(vc))
	for k, v := range vc {
		if CortexIDKey(k) {
			clean[k] = v
		}
	}
	return clean
}

// Increment bumps the counter for the given peer.
func (vc VClock) Increment(peer string) {
	vc[peer]++
}

// Clone returns a deep copy of the vector clock.
func (vc VClock) Clone() VClock {
	c := make(VClock, len(vc))
	for k, v := range vc {
		c[k] = v
	}
	return c
}

// Merge returns a new VClock with the component-wise max of two clocks.
func Merge(a, b VClock) VClock {
	result := make(VClock, len(a)+len(b))
	for k, v := range a {
		result[k] = v
	}
	for k, v := range b {
		if v > result[k] {
			result[k] = v
		}
	}
	return result
}

// MergeCapped is like Merge but returns an error when the merged clock
// would exceed MaxVClockEntries. Use this on the federation ingest path
// to prevent a malicious peer from inflating the local clock with
// synthetic cortex IDs.
func MergeCapped(a, b VClock) (VClock, error) {
	merged := Merge(a, b)
	if len(merged) > MaxVClockEntries {
		return nil, fmt.Errorf(
			"vector clock too large (%d entries, max %d): possible clock inflation attack",
			len(merged), MaxVClockEntries)
	}
	return merged, nil
}

// Compare returns the causal relationship between two clocks:
//
//	-1 means a happened-before b
//	 0 means concurrent (no causal relationship — potential conflict)
//	+1 means b happened-before a
func Compare(a, b VClock) int {
	aLessOrEq, bLessOrEq := true, true
	all := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		all[k] = struct{}{}
	}
	for k := range b {
		all[k] = struct{}{}
	}
	for k := range all {
		if a[k] > b[k] {
			aLessOrEq = false // a is NOT <= b at this component
		}
		if b[k] > a[k] {
			bLessOrEq = false // b is NOT <= a at this component
		}
	}
	switch {
	case aLessOrEq && !bLessOrEq:
		return -1
	case bLessOrEq && !aLessOrEq:
		return +1
	default:
		return 0
	}
}
