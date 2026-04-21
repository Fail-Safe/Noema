package consolidation

import (
	crand "crypto/rand"
	"encoding/json"
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

// RankEntry is one peer's advertised consolidation eligibility for the
// current window. ObservedAt is the RFC3339 UTC timestamp at which the
// entry was last set — the election uses it to enforce a quiet-period
// guard so too-fresh entries from an in-flight federation sync don't
// skew the winner choice.
type RankEntry struct {
	CortexID   string `json:"cortex_id"`
	Rank       int    `json:"rank"`
	ObservedAt string `json:"observed_at"`
}

// Federation-state keys. The local rank lives under a single key; each
// remote peer's last-observed rank lives under a peer-scoped key that
// mirrors the existing peer:<name>:* convention in federation/state.go.
const localRankKey = "consolidation:rank"

func peerRankKey(name string) string {
	return "peer:" + name + ":consolidation_rank"
}

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
func ElectWinner(entries []RankEntry, minAge time.Duration, now time.Time) string {
	eligible := make([]RankEntry, 0, len(entries))
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

// ReadLocalRank returns this cortex's currently-advertised rank entry.
// A missing or malformed value is returned as a zero-value RankEntry
// (Rank == RankIneligible); callers treat "no data yet" the same as
// "explicitly ineligible".
func ReadLocalRank(s *federation.State) (RankEntry, error) {
	val, err := s.Get(localRankKey)
	if err != nil {
		return RankEntry{}, err
	}
	if val == "" {
		return RankEntry{}, nil
	}
	var e RankEntry
	if err := json.Unmarshal([]byte(val), &e); err != nil {
		return RankEntry{}, nil
	}
	return e, nil
}

// WriteLocalRank persists this cortex's rank entry. Callers advertise the
// same entry outward via the heartbeat channel (announce_peer) so remote
// peers observe the same value within one sync interval.
func WriteLocalRank(s *federation.State, e RankEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.Set(localRankKey, string(data))
}

// ReadPeerRank returns a remote peer's last-observed rank entry. Same
// missing/malformed semantics as ReadLocalRank.
func ReadPeerRank(s *federation.State, peerName string) (RankEntry, error) {
	val, err := s.Get(peerRankKey(peerName))
	if err != nil {
		return RankEntry{}, err
	}
	if val == "" {
		return RankEntry{}, nil
	}
	var e RankEntry
	if err := json.Unmarshal([]byte(val), &e); err != nil {
		return RankEntry{}, nil
	}
	return e, nil
}

// WritePeerRank records a remote peer's advertised rank entry. Called
// from the federation heartbeat path on every successful exchange that
// carries rank data.
func WritePeerRank(s *federation.State, peerName string, e RankEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.Set(peerRankKey(peerName), string(data))
}
