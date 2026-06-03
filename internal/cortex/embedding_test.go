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
