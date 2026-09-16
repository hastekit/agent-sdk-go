package middleware

import (
	"context"
	"fmt"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	gatewaymiddleware "github.com/hastekit/agent-sdk-go/pkg/gateway/middleware"
)

// FallbackConfig lists explicit Provider/model targets after the agent's model.
// Credentials are resolved by the agent's configured model client.
type FallbackConfig struct {
	Targets      []string
	Fallbackable func(error) bool
}
type Fallback struct {
	agents.NoopMiddleware
	cfg FallbackConfig
}

var _ agents.Middleware = (*Fallback)(nil)

func NewFallback(cfg FallbackConfig) *Fallback {
	cfg.Targets = append([]string(nil), cfg.Targets...)
	if cfg.Fallbackable == nil {
		cfg.Fallbackable = gatewaymiddleware.DefaultFallbackable
	}
	return &Fallback{cfg: cfg}
}
func NewFallbackModels(targets ...string) *Fallback {
	return NewFallback(FallbackConfig{Targets: targets})
}
func (m *Fallback) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		// Check configuration before making a paid request.
		for _, target := range m.cfg.Targets {
			parts := strings.SplitN(target, "/", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, policyFinished(fmt.Errorf("fallback target %q must be Provider/model", target))
			}
		}
		for attempt := 0; ; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			c, r := *call, *request
			if attempt > 0 {
				c.Target = m.cfg.Targets[attempt-1]
				c.Model = c.Target
				r.Model = c.Target
			}
			response, err := next(ctx, &c, &r)
			if err == nil {
				return response, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if cannotRepeat(ctx, err) || attempt >= len(m.cfg.Targets) || !m.cfg.Fallbackable(err) {
				return nil, policyFinished(err)
			}
		}
	}
}
