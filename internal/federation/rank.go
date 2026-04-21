package federation

import "encoding/json"

// RankEntry is a peer's advertised consolidation-eligibility bid for the
// current window. Zero Rank means the peer is not participating; a
// positive Rank is their randomized leadership bid. See
// consolidation-plan.md §14 for the coordination protocol that consumes
// these entries.
//
// Lives in the federation package alongside PeerHealth because rank is
// per-peer state advertised via the standard heartbeat and stored in the
// same federation_state kv table. Consolidation-specific logic (bid
// generation, winner election) stays in internal/consolidation.
type RankEntry struct {
	CortexID   string `json:"cortex_id"`
	Rank       int    `json:"rank"`
	ObservedAt string `json:"observed_at"` // RFC3339 UTC
}

// localRankKey is the federation_state key for this cortex's own current
// advertised rank. Only one local entry per cortex, so no peer suffix.
const localRankKey = "consolidation:rank"

// PeerRankKey returns the federation_state key under which a remote
// peer's last-observed rank is cached. Parallels PeerCursorKey /
// PeerHealthKey from state.go.
func PeerRankKey(name string) string {
	return "peer:" + name + ":consolidation_rank"
}

// GetLocalRank returns this cortex's currently-advertised rank. A
// missing or malformed value is returned as the zero-value RankEntry
// (Rank == 0). Rank is advisory; callers never block on load errors.
func (s *State) GetLocalRank() (RankEntry, error) {
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

// SetLocalRank persists this cortex's current rank entry.
func (s *State) SetLocalRank(e RankEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.Set(localRankKey, string(data))
}

// GetPeerRank returns a remote peer's last-observed rank entry. Same
// missing / malformed semantics as GetLocalRank.
func (s *State) GetPeerRank(name string) (RankEntry, error) {
	val, err := s.Get(PeerRankKey(name))
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

// SetPeerRank records a remote peer's advertised rank. Called from the
// syncer on every successful identity handshake that carries a rank
// field.
func (s *State) SetPeerRank(name string, e RankEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.Set(PeerRankKey(name), string(data))
}
