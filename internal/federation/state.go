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

// GetPeerState loads the runtime state for a peer.
func (s *State) GetPeerState(name, endpoint string) (PeerState, error) {
	ps := PeerState{Name: name, Endpoint: endpoint}
	var err error
	ps.LastEvent, err = s.Get(PeerCursorKey(name))
	if err != nil {
		return ps, err
	}
	ps.LastSeen, err = s.Get(PeerSeenKey(name))
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
