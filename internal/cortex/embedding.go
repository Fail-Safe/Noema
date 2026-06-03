package cortex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// embeddingCodecVersion prefixes every stored embedding BLOB so a future
// encoding (e.g. int8 quantization) can be introduced without ambiguity:
// the decoder branches on this first byte.
const embeddingCodecVersion byte = 1

// encodeEmbedding serializes a float32 vector to the BLOB layout stored in
// trace_embeddings.embedding: a 1-byte version tag followed by little-
// endian float32 elements. Pure Go, no extension required — similarity is
// computed in Go over the decoded slices.
func encodeEmbedding(v []float32) []byte {
	buf := make([]byte, 1+4*len(v))
	buf[0] = embeddingCodecVersion
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[1+4*i:], math.Float32bits(f))
	}
	return buf
}

// decodeEmbedding reverses encodeEmbedding. It rejects an unknown version
// tag and a payload whose length isn't a whole number of float32s.
func decodeEmbedding(b []byte) ([]float32, error) {
	if len(b) == 0 {
		return nil, errors.New("empty embedding blob")
	}
	if b[0] != embeddingCodecVersion {
		return nil, fmt.Errorf("unknown embedding codec version %d", b[0])
	}
	payload := b[1:]
	if len(payload)%4 != 0 {
		return nil, fmt.Errorf("embedding blob payload length %d is not a multiple of 4", len(payload))
	}
	out := make([]float32, len(payload)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(payload[4*i:]))
	}
	return out, nil
}

// normalizeEmbedding scales v to unit L2 norm in place so that cosine
// similarity reduces to a plain dot product at query time. A zero vector
// is left unchanged (no division by zero).
func normalizeEmbedding(v []float32) {
	var ss float64
	for _, f := range v {
		ss += float64(f) * float64(f)
	}
	if ss == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(ss))
	for i := range v {
		v[i] *= inv
	}
}
