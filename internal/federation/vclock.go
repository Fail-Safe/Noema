package federation

// VClock is a vector clock: one counter per known peer.
type VClock map[string]uint64

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
