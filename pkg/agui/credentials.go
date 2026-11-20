package agui

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"net/http"
	"net/url"
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
	mux.Handle("GET /mcp", withTrailingSlash(handler))
	mux.Handle("GET /mcp/{$}", handler)
	mux.Handle("PUT /mcp/{server}", handler)
	mux.Handle("DELETE /mcp/{server}", handler)
	mux.Handle("GET /mcp/{server}/connect", handler)
	mux.Handle("GET /mcp/{server}/callback", handler)
}

// withTrailingSlash serves a collection root requested without its trailing
// slash as if the slash were there. Stripping "/mcp" from a bare "/mcp" leaves
// an empty path, which the inner ServeMux would answer with a redirect that
// loses every prefix stripped before it.
func withTrailingSlash(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path += "/"
		if r2.URL.RawPath != "" {
			r2.URL.RawPath += "/"
		}
		h.ServeHTTP(w, r2)
	})
}
