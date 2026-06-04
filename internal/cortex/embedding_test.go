package cortex

import (
	"math"
	"testing"
)

func TestEmbeddingCodec_RoundTrip(t *testing.T) {
	cases := [][]float32{
		{},
		{0},
		{1.5, -2.25, 0, 3.0e-8, 1234.5},
	}
	for _, want := range cases {
		blob := encodeEmbedding(want)
		if len(blob) != 1+4*len(want) {
			t.Fatalf("blob len = %d, want %d", len(blob), 1+4*len(want))
		}
		if blob[0] != embeddingCodecVersion {
			t.Fatalf("version byte = %d, want %d", blob[0], embeddingCodecVersion)
		}
		got, err := decodeEmbedding(blob)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("decoded len = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("elem %d = %v, want %v", i, got[i], want[i])
			}
		}
	}
}

func TestDecodeEmbedding_Errors(t *testing.T) {
	if _, err := decodeEmbedding(nil); err == nil {
		t.Error("empty blob should error")
	}
	if _, err := decodeEmbedding([]byte{0x02, 0, 0, 0, 0}); err == nil {
		t.Error("unknown version byte should error")
	}
	if _, err := decodeEmbedding([]byte{embeddingCodecVersion, 0, 0, 0}); err == nil {
		t.Error("payload not a multiple of 4 should error")
	}
}

func TestNormalizeEmbedding(t *testing.T) {
	v := []float32{3, 4} // norm 5
	normalizeEmbedding(v)
	var ss float64
	for _, f := range v {
		ss += float64(f) * float64(f)
	}
	if math.Abs(math.Sqrt(ss)-1) > 1e-6 {
		t.Errorf("normalized norm = %v, want 1", math.Sqrt(ss))
	}

	zero := []float32{0, 0, 0}
	normalizeEmbedding(zero) // must not divide by zero
	for i, f := range zero {
		if f != 0 {
			t.Errorf("zero vector elem %d changed to %v", i, f)
		}
	}
}

// TestEmbeddingCodec_LargeVectorAndSpecials exercises a realistic 1536-dim
// vector and confirms the bit-exact path preserves non-finite and signed-
// zero values (a naive codec could corrupt these). Storage must be
// faithful even if such values are later rejected at ingest.
func TestEmbeddingCodec_LargeVectorAndSpecials(t *testing.T) {
	const dim = 1536
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(i)*0.001 - 0.5
	}
	v[0] = float32(math.NaN())
	v[1] = float32(math.Inf(1))
	v[2] = float32(math.Inf(-1))
	v[3] = float32(math.Copysign(0, -1)) // -0.0

	blob := encodeEmbedding(v)
	if len(blob) != 1+4*dim {
		t.Fatalf("blob len = %d, want %d", len(blob), 1+4*dim)
	}
	got, err := decodeEmbedding(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != dim {
		t.Fatalf("decoded len = %d, want %d", len(got), dim)
	}
	if !math.IsNaN(float64(got[0])) {
		t.Errorf("NaN not preserved: %v", got[0])
	}
	if !math.IsInf(float64(got[1]), 1) || !math.IsInf(float64(got[2]), -1) {
		t.Errorf("Inf not preserved: %v %v", got[1], got[2])
	}
	// bit-exact check for -0.0 and the rest
	for i := 4; i < dim; i++ {
		if math.Float32bits(got[i]) != math.Float32bits(v[i]) {
			t.Fatalf("elem %d bits differ: got %x want %x", i, math.Float32bits(got[i]), math.Float32bits(v[i]))
		}
	}
	if math.Float32bits(got[3]) != math.Float32bits(v[3]) {
		t.Errorf("-0.0 not preserved: bits %x", math.Float32bits(got[3]))
	}
}

// FuzzDecodeEmbedding guards the decoder against arbitrary bytes (a
// corrupted or foreign-written trace_embeddings row). It must never panic;
// on success the length/version invariants must hold.
func FuzzDecodeEmbedding(f *testing.F) {
	f.Add(encodeEmbedding([]float32{1, 2, 3}))
	f.Add(encodeEmbedding(nil))
	f.Add([]byte{})
	f.Add([]byte{embeddingCodecVersion})
	f.Add([]byte{0x02, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		out, err := decodeEmbedding(b)
		if err != nil {
			return
		}
		if len(b) == 0 || b[0] != embeddingCodecVersion {
			t.Fatalf("decode accepted invalid header: %v", b)
		}
		if len(b) != 1+4*len(out) {
			t.Fatalf("len invariant broken: blob %d, out %d", len(b), len(out))
		}
	})
}
