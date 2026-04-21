package consolidation

import (
	crand "crypto/rand"
	"math/big"
	"sort"
	"time"

	"github.com/Fail-Safe/Noema/internal/federation"
)

// Consolidation-eligibility rank constants. Zero means this peer is not
// participating in the current window; a non-zero value is the peer's bid
// for leadership. The 1..99 range is deliberately small enough to read at
// a glance in status output — collisions are expected (birthday-paradox
// ~10% at 5 peers, ~35% at 10) and resolved by the cortex-ID tiebreak
// in ElectWinner. See consolidation-plan.md §14.
const (
	RankIneligible = 0
	RankMin        = 1
	RankMax        = 99
)

// GenerateRank returns a fresh random rank in [RankMin, RankMax]. Uses
// crypto/rand so two peers with a shared math/rand seed (unlikely in
// production but plausible in fuzz or replay testing) don't generate
// identical bids and defeat the tiebreak.
func GenerateRank() (int, error) {
	span := big.NewInt(int64(RankMax - RankMin + 1))
	n, err := crand.Int(crand.Reader, span)
	if err != nil {
		return 0, err
	}
	return int(n.Int64()) + RankMin, nil
}

// ElectWinner returns the CortexID of the elected peer, or the empty
// string if no entry qualifies. Filters applied in order:
//
//  1. Rank <= RankIneligible (peer is not participating this window).
//  2. ObservedAt is empty or unparseable (defensive: don't elect a peer
//     we don't have a timestamp for).
//  3. ObservedAt is fresher than minAge (quiet-period guard; see §14 of
//     the plan).
//
// Survivors are sorted by rank descending, then CortexID descending.
// Lex-max CortexID breaks rank ties deterministically across peers.
func ElectWinner(entries []federation.RankEntry, minAge time.Duration, now time.Time) string {
	eligible := make([]federation.RankEntry, 0, len(entries))
	for _, e := range entries {
		if e.Rank <= RankIneligible {
			continue
		}
		if e.ObservedAt == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.ObservedAt)
		if err != nil {
			continue
		}
		if now.Sub(ts) < minAge {
			continue
		}
		eligible = append(eligible, e)
	}
	if len(eligible) == 0 {
		return ""
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Rank != eligible[j].Rank {
			return eligible[i].Rank > eligible[j].Rank
		}
		return eligible[i].CortexID > eligible[j].CortexID
	})
	return eligible[0].CortexID
}
