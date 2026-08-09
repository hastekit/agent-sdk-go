// Package web embeds a ready-made browser chat client for the AG-UI
// protocol and serves it alongside the protocol endpoints.
//
// The static UI is embedded into the binary via go:embed, so importing
// this package needs no Node toolchain and no separate frontend deploy.
// Serve mirrors http.ListenAndServe:
//
//	client, _ := hastekit.NewWithOptions(...)
//	client.NewAgent(...)
//	if err := web.Serve(":8080", client); err != nil { log.Fatal(err) }
//
// Then open http://localhost:8080. The default UI (index.html) is a
// CopilotKit chat: it lists registered agents, shows a sidebar of
// prior conversations to resume, streams assistant text/reasoning/
// tool calls live, and renders approval cards inline for human-in-the-
// loop pauses.
//
// The CopilotKit UI is a Vite/React build whose output is committed to
// static/ (see ui/ for the source and rebuild steps). CopilotKit v2
// cannot be loaded from a public ESM CDN — its dependency graph breaks
// esm.sh/jsDelivr — so it is bundled; to keep the embedded weight down
// (~3MB rather than ~17MB) the build swaps CopilotKit's heavy markdown
// renderer (Shiki + Mermaid + Cytoscape) for a lightweight one. A
// zero-dependency vanilla UI that needs no framework at all is embedded
// at /basic.html as an offline fallback and talks to the same endpoints.
//
// Handler returns the same surface as an http.Handler for mounting
// into an existing server:
//
//	GET  /                            → embedded CopilotKit chat UI
//	GET  /basic.html                  → offline (no-CDN) fallback UI
//	GET  /api/agui/agents             → registered agent names
//	POST /api/agui/agents/{name}/run  → AG-UI run endpoint (SSE)
//	POST /api/agui/agents/{name}/stop → stop a run in flight
//	GET  /api/agui/agents/{name}/threads                   → conversation list
//	GET  /api/agui/agents/{name}/threads/{thread}/messages → thread history
//	GET  /api/agui/agents/{name}/threads/{thread}/stream   → rejoin a run in flight
//
// A thread streams on the same channel every turn, so the stream endpoint
// finds a run without the client having kept anything: it replays the run
// so far and then follows it live, which is how a client that reloaded or
// navigated away picks one back up.
//
// A run posted for a thread that is already running folds into it: the
// server answers 204 and the reply appears on that run's stream. That is
// how a client steers a run in flight — send the new turn and keep reading
// the stream it already has.
//
// The stop endpoint takes the stream id the run returned (the
// X-Stream-Id header, or the streamId CUSTOM event) as {"streamId": …}
// or ?streamId=…, and answers 202 once the stop is recorded. The run ends
// on its own SSE connection with RUN_FINISHED. It travels via the agent's
// broker, so with a shared broker it need not reach the replica streaming
// that run.
//
// The /api/agui/* endpoints speak the canonical AG-UI protocol, so
// external clients (CopilotKit, raw @ag-ui/client) can target them
// too — the embedded UIs are just two consumers.
package web

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/hastekit/hastekit-sdk-go/pkg/agui"
)

//go:embed static
var staticFS embed.FS

// APIPrefix is the path the AG-UI protocol endpoints are mounted
// under. The embedded UI is built against it; external AG-UI clients
// should use it too.
const APIPrefix = "/api/agui"

// Handler serves the embedded AG-UI chat client over every agent in
// the registry, with the AG-UI protocol endpoints mounted under
// APIPrefix. *hastekit.SDK satisfies agui.Registry.
func Handler(registry agui.Registry, opts ...agui.Option) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(APIPrefix+"/", http.StripPrefix(APIPrefix, agui.NewHandler(registry, opts...)))

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Unreachable: the embed directive guarantees the directory
		// exists at compile time.
		panic(err)
	}
	mux.Handle("/", http.FileServerFS(static))
	return mux
}

// Serve runs the embedded AG-UI chat client on addr, blocking like
// http.ListenAndServe.
func Serve(addr string, registry agui.Registry, opts ...agui.Option) error {
	return http.ListenAndServe(addr, Handler(registry, opts...))
}
