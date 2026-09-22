package hastekitgateway

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"strings"
)

// NewSandboxClient routes lifecycle, execution, and binary file operations through
// the gateway's configured provider. It never connects directly to a pod address.
func (c *Config) NewSandboxClient() *sandbox.HTTPProvider {
	return &sandbox.HTTPProvider{BaseURL: strings.TrimRight(c.Endpoint, "/") + "/api/sandbox", Client: c.HttpClient}
}
