package federation

import "time"

const DefaultInterval = 30 * time.Second

// PeerConfig is a peer entry as declared in cortex.md.
type PeerConfig struct {
	Name     string `yaml:"name"`
	Endpoint string `yaml:"endpoint"`
	CA       string `yaml:"ca,omitempty"` // path to CA certificate for TLS verification
}

// Config holds federation settings from cortex.md.
type Config struct {
	Peers    []PeerConfig  `yaml:"peers,omitempty"`
	Interval time.Duration // parsed from string
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
}
