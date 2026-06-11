package federation

import (
	"database/sql"
	"encoding/json"
)

// State provides read/write access to the federation_state table.
type State struct {
	db *sql.DB
}

func NewState(db *sql.DB) *State {
	return &State{db: db}
}

func (s *State) Get(key string) (string, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM federation_state WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

func (s *State) Set(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO federation_state (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// Delete removes a single key from federation_state. Missing keys are not
// an error — Delete is used by `noema federation reset-peer` to clear pin /
// cursor / last_seen rows that may or may not exist depending on whether
// the peer has ever been contacted, so the operation has to be idempotent.
func (s *State) Delete(key string) error {
	_, err := s.db.Exec(`DELETE FROM federation_state WHERE key = ?`, key)
	return err
}

// GetClock loads the vector clock from federation_state.
func (s *State) GetClock() (VClock, error) {
	val, err := s.Get("vclock")
	if err != nil {
		return nil, err
	}
	if val == "" {
		return make(VClock), nil
	}
	var vc VClock
	if err := json.Unmarshal([]byte(val), &vc); err != nil {
		return make(VClock), nil
	}
	return vc, nil
}

// SetClock persists the vector clock to federation_state.
func (s *State) SetClock(vc VClock) error {
	data, err := json.Marshal(vc)
	if err != nil {
		return err
	}
	return s.Set("vclock", string(data))
}

// PeerCursorKey returns the federation_state key for a peer's last_event cursor.
func PeerCursorKey(peerName string) string {
	return "peer:" + peerName + ":last_event"
}

// PeerSeenKey returns the federation_state key for a peer's last_seen time.
func PeerSeenKey(peerName string) string {
	return "peer:" + peerName + ":last_seen"
}

// PeerUsageCursorKey returns the federation_state key for a peer's
// last_usage cursor (highest trace_usage.updated_at this peer's ever
// seen from the named peer). Separate from the event cursor because
// the two streams advance independently — a peer can be quiet on
// mutations but chatty on reads, or vice versa.
func PeerUsageCursorKey(peerName string) string {
	return "peer:" + peerName + ":last_usage"
}

// PeerCortexIDKey returns the federation_state key under which a peer's verified
// cortex ULID is pinned after the first successful identity handshake. The
// syncer refuses to talk to a peer whose advertised ID has changed from what
// is stored here — see docs/design/cortex-uuid-plan.md.
func PeerCortexIDKey(peerName string) string {
	return "peer:" + peerName + ":cortex_id"
}

// PeerHealthKey returns the federation_state key holding the JSON-
// encoded PeerHealth for this peer. Deliberately separate from the
// other peer:* keys so health snapshots can be reset without touching
// the cursor or pinned identity.
func PeerHealthKey(peerName string) string {
	return "peer:" + peerName + ":health"
}

// GetPeerHealth loads the structured health snapshot for a peer. A
// missing key or empty value both return an empty PeerHealth — callers
// treat that as "no data yet" identically to "first poll is about to
// happen". Malformed JSON is tolerated the same way: the snapshot is
// advisory, not load-bearing, and a parse error shouldn't block the
// CLI from rendering the rest of federation status.
func (s *State) GetPeerHealth(name string) (PeerHealth, error) {
	val, err := s.Get(PeerHealthKey(name))
	if err != nil {
		return PeerHealth{}, err
	}
	if val == "" {
		return PeerHealth{}, nil
	}
	var h PeerHealth
	if err := json.Unmarshal([]byte(val), &h); err != nil {
		return PeerHealth{}, nil
	}
	return h, nil
}

// SetPeerHealth persists the snapshot for a peer. Callers build the
// full PeerHealth (usually by reading the previous value, adjusting,
// and writing back) so partial updates don't need their own API.
func (s *State) SetPeerHealth(name string, h PeerHealth) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return s.Set(PeerHealthKey(name), string(data))
}

// GetPeerState loads the runtime state for a peer.
func (s *State) GetPeerState(name, endpoint string) (PeerState, error) {
	ps := PeerState{Name: name, Endpoint: endpoint}
	var err error
	ps.LastEvent, err = s.Get(PeerCursorKey(name))
	if err != nil {
		return ps, err
	}
	ps.LastSeen, err = s.Get(PeerSeenKey(name))
	if err != nil {
		return ps, err
	}
	ps.CortexID, err = s.Get(PeerCortexIDKey(name))
	if err != nil {
		return ps, err
	}
	ps.Health, err = s.GetPeerHealth(name)
	return ps, err
}

// SetPeerCursor updates the last synced event cursor for a peer.
func (s *State) SetPeerCursor(name, eventID string) error {
	return s.Set(PeerCursorKey(name), eventID)
}

// SetPeerSeen updates the last seen time for a peer.
func (s *State) SetPeerSeen(name, timestamp string) error {
	return s.Set(PeerSeenKey(name), timestamp)
}

// SetPeerCortexID pins a peer's verified cortex ULID. Should only be called
// after the cortex_identity handshake has succeeded once.
func (s *State) SetPeerCortexID(name, cortexID string) error {
	return s.Set(PeerCortexIDKey(name), cortexID)
}

// CortexPubKeyKey returns the federation_state key under which a cortex's
// pinned Ed25519 public key is stored. It is keyed on cortex_id rather than
// peer name on purpose: events gossip transitively, so the verifier must be
// able to look up the signing key for any originating cortex_id it sees,
// including third parties it has no direct peer entry for.
func CortexPubKeyKey(cortexID string) string {
	return "cortexkey:" + cortexID
}

// GetCortexPubKey returns the pinned public key for a cortex_id, or "" if none
// is pinned yet.
func (s *State) GetCortexPubKey(cortexID string) (string, error) {
	return s.Get(CortexPubKeyKey(cortexID))
}

// SetCortexPubKey pins (or overwrites) the public key for a cortex_id. Callers
// own the trust policy — whether an overwrite is allowed (rotation), refused
// (handshake identity-change), or flagged (replay conflict) — and call this
// only once they have decided to (re-)pin.
func (s *State) SetCortexPubKey(cortexID, pubkey string) error {
	return s.Set(CortexPubKeyKey(cortexID), pubkey)
}
