package agui

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"net/http"
)

// mountMCPCredentials shares the chat namespace with connector management and OAuth routes.
func (o options) mountMCPCredentials(mux *http.ServeMux) {
	if o.mcpStore == nil {
		return
	}

	// Install trusted identity last so handler options cannot override chat ownership.
	options := append([]mcpclient.HandlerOption(nil), o.mcpOptions...)
	options = append(options, mcpclient.WithNamespaceResolver(func(r *http.Request) (string, error) {
		return requestNamespace(r), nil
	}))
	handler := o.withNamespace(http.StripPrefix("/mcp", mcpclient.NewHandler(o.mcpStore, options...)))
	mux.Handle("GET /mcp/{$}", handler)
	mux.Handle("PUT /mcp/{server}", handler)
	mux.Handle("DELETE /mcp/{server}", handler)
	mux.Handle("GET /mcp/{server}/connect", handler)
	mux.Handle("GET /mcp/{server}/callback", handler)
}
