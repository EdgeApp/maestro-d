package daemon

// aicheck.go: the keys a session's assertWithAI and assertNoDefectsWithAI
// spend. A daemon's own config.json ai block wins: a hub answers for its
// own devices with its own keys and ignores the keys a client sent. A
// daemon with no block uses the keys its attach carried, which is how a
// local browser daemon gets its runner's keys.

import (
	"github.com/devicelab-dev/maestro-runner/pkg/aicheck"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// AIJevURL and AIAnthropicURL override the AI checks' endpoints (tests).
var (
	AIJevURL       string
	AIAnthropicURL string
)

// aiChecker is a session's checker. The config is read at every check, so
// an edit to the ai block, or a key file, takes effect without a restart.
func (s *Server) aiChecker(attach *AttachAI) *aicheck.Checker {
	var fromAttach aicheck.Keys
	if attach != nil {
		fromAttach = aicheck.Keys{Jev: attach.JevKey, Anthropic: attach.AnthropicKey}
	}
	return &aicheck.Checker{
		JevURL:       AIJevURL,
		AnthropicURL: AIAnthropicURL,
		Keys: func() (aicheck.Keys, error) {
			cfg, err := netshape.LoadConfig(DeviceConfigPath(s.cfg.DeviceConfig))
			if err != nil {
				return aicheck.Keys{}, err
			}
			if cfg.AI == nil {
				return fromAttach, nil
			}
			k, err := cfg.AI.ReadKeys()
			if err != nil {
				return aicheck.Keys{}, err
			}
			return aicheck.Keys{Jev: k.Jev, Anthropic: k.Anthropic, Own: true, MaxBudgetUSD: cfg.AI.MaxBudgetUSDPerCall}, nil
		},
	}
}
