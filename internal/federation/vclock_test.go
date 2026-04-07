package federation

import "testing"

func TestVClock_Increment(t *testing.T) {
	vc := VClock{"a": 5, "b": 3}
	vc.Increment("a")
	if vc["a"] != 6 {
		t.Errorf("a = %d, want 6", vc["a"])
	}
	vc.Increment("c") // new peer
	if vc["c"] != 1 {
		t.Errorf("c = %d, want 1", vc["c"])
	}
}

func TestVClock_Clone(t *testing.T) {
	vc := VClock{"a": 1, "b": 2}
	c := vc.Clone()
	c["a"] = 99
	if vc["a"] != 1 {
		t.Error("Clone must not alias the original")
	}
}

func TestMerge(t *testing.T) {
	a := VClock{"a": 10, "b": 7, "c": 3}
	b := VClock{"a": 9, "b": 8, "c": 3}
	m := Merge(a, b)
	want := VClock{"a": 10, "b": 8, "c": 3}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("Merge[%s] = %d, want %d", k, m[k], v)
		}
	}
}

func TestMerge_DisjointKeys(t *testing.T) {
	a := VClock{"a": 5}
	b := VClock{"b": 3}
	m := Merge(a, b)
	if m["a"] != 5 || m["b"] != 3 {
		t.Errorf("Merge = %v, want {a:5, b:3}", m)
	}
}

func TestCompare_HappenedBefore(t *testing.T) {
	a := VClock{"x": 1, "y": 2}
	b := VClock{"x": 2, "y": 3}
	if got := Compare(a, b); got != -1 {
		t.Errorf("Compare = %d, want -1 (a before b)", got)
	}
}

func TestCompare_HappenedAfter(t *testing.T) {
	a := VClock{"x": 3, "y": 4}
	b := VClock{"x": 2, "y": 3}
	if got := Compare(a, b); got != +1 {
		t.Errorf("Compare = %d, want +1 (b before a)", got)
	}
}

func TestCompare_Concurrent(t *testing.T) {
	a := VClock{"x": 3, "y": 2}
	b := VClock{"x": 2, "y": 3}
	if got := Compare(a, b); got != 0 {
		t.Errorf("Compare = %d, want 0 (concurrent)", got)
	}
}

func TestCompare_Equal(t *testing.T) {
	a := VClock{"x": 5, "y": 5}
	b := VClock{"x": 5, "y": 5}
	if got := Compare(a, b); got != 0 {
		t.Errorf("Compare = %d, want 0 (equal)", got)
	}
}

func TestCompare_MissingKeys(t *testing.T) {
	a := VClock{"x": 1}
	b := VClock{"x": 1, "y": 1}
	// a <= b on all components (a["y"] is 0, b["y"] is 1)
	if got := Compare(a, b); got != -1 {
		t.Errorf("Compare = %d, want -1 (a before b, missing key)", got)
	}
}
