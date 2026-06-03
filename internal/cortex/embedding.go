package cortex

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Embedder turns text into vectors. Defined here (not imported from
// consolidation) so the cortex package stays free of an import cycle —
// consolidation.HTTPLLMClient satisfies this structurally and is injected
// by the CLI/serve layer, which may import both packages.
type Embedder interface {
	Embed(ctx context.Context, model string, inputs []string) ([][]float32, error)
}

// embeddingText builds the string sent to the embedding model for a trace:
// title and body joined, trimmed, and truncated to maxChars runes to stay
// within model input limits. A blank body embeds the title alone (and vice
// versa). Truncation is on a rune boundary so a multibyte character is
// never split.
func embeddingText(title, body string, maxChars int) string {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	var s string
	switch {
	case body == "":
		s = title
	case title == "":
		s = body
	default:
		s = title + "\n\n" + body
	}
	if maxChars > 0 {
		if r := []rune(s); len(r) > maxChars {
			s = string(r[:maxChars])
		}
	}
	return s
}

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
