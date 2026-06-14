// Package check assembles the agent self-check reported to SentNodes and
// decides whether the hard prerequisites are met.
package check

type Signals struct {
	Volume    bool // node home mounted + config.toml readable + node home writable
	Config    bool // config.toml parsed with required fields + writable (changes applyable)
	Keyring   bool // operator key loaded + address derived
	Backend   bool // keyring backend == test
	Container bool // node container discovered
	Server    bool // SentNodes reachable + token valid
}

func (s Signals) Map() map[string]bool {
	return map[string]bool{
		"volume":    s.Volume,
		"config":    s.Config,
		"keyring":   s.Keyring,
		"backend":   s.Backend,
		"container": s.Container,
		"server":    s.Server,
	}
}
