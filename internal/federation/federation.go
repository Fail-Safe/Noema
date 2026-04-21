package federation

import "time"

const DefaultInterval = 30 * time.Second

// Peer mode constants — checked by the syncer to skip paused peers.
const (
	PeerModeSync   = "sync"
	PeerModePaused = "paused"
)

// PeerConfig is a peer entry as declared in cortex.md.
type PeerConfig struct {
	Name     string `yaml:"name"`
	Endpoint string `yaml:"endpoint"`
	CA       string `yaml:"ca,omitempty"` // path to CA certificate for TLS verification
	Mode     string `yaml:"mode,omitempty"` // sync | paused
}

// Config holds federation settings from cortex.md.
type Config struct {
	Mode     string        // federation-level mode: sync | publish | subscribe
	Peers    []PeerConfig  `yaml:"peers,omitempty"`
	Interval time.Duration // parsed from string

	// SharedKey is the MCP bearer token this syncer attaches to every
	// outbound request as "Authorization: Bearer <SharedKey>". Empty
	// means open mode (no header sent). Populated at startup from
	// cortex.LoadAccessKey; never surfaced in YAML, logs, or events.
	SharedKey string `yaml:"-"`
}

// EffectiveInterval returns the configured interval or the default.
func (c Config) EffectiveInterval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultInterval
}

// PeerState is the runtime state of a known peer.
type PeerState struct {
	Name      string
	Endpoint  string
	LastSeen  string // RFC3339, "" if never reached
	LastEvent string // ULID of last synced event, "" if never synced
	CortexID  string // pinned ULID after first successful identity handshake
	Health    PeerHealth
}
