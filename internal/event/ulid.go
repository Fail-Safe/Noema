package event

import (
	"crypto/rand"
	"sync"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu      sync.Mutex
	lastMs  uint64
	lastRnd [10]byte
)

// NewULID generates a monotonic ULID: 10 chars of millisecond timestamp + 16
// chars of randomness, Crockford base32 encoded. Within the same millisecond,
// the random portion is incremented to guarantee lexicographic ordering.
func NewULID() string {
	mu.Lock()
	defer mu.Unlock()

	ms := uint64(time.Now().UnixMilli())

	if ms == lastMs {
		// Same millisecond — increment the random portion.
		incrementRnd()
	} else {
		// New millisecond — fresh random value.
		lastMs = ms
		if _, err := rand.Read(lastRnd[:]); err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
	}

	var buf [26]byte
	encodeTimestamp(buf[:], ms)
	encodeRandom(buf[:], lastRnd)
	return string(buf[:])
}

func encodeTimestamp(buf []byte, ms uint64) {
	for i := 9; i >= 0; i-- {
		buf[i] = crockford[ms&0x1F]
		ms >>= 5
	}
}

func encodeRandom(buf []byte, rnd [10]byte) {
	// Work on a copy so we don't mutate lastRnd during bit-shifting.
	tmp := rnd
	for i := 15; i >= 0; i-- {
		buf[10+i] = crockford[tmp[9]&0x1F]
		carry := byte(0)
		for j := 0; j < 10; j++ {
			next := tmp[j] << 3
			tmp[j] = carry | (tmp[j] >> 5)
			carry = next
		}
	}
}

// IsValidULID reports whether s is a well-formed 26-character Crockford
// base32 ULID. It does not check whether the timestamp or random portion
// is semantically valid — only the length and character set.
func IsValidULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
		// Crockford base32 excludes I, L, O, U.
		if c == 'I' || c == 'L' || c == 'O' || c == 'U' {
			return false
		}
	}
	return true
}

func incrementRnd() {
	for i := 9; i >= 0; i-- {
		lastRnd[i]++
		if lastRnd[i] != 0 {
			return // no carry
		}
	}
	// Overflow of 80-bit random — extremely unlikely. Advance time.
	lastMs++
}
