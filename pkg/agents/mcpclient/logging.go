package mcpclient

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// The client logs, through the default slog logger, what it sends MCP servers
// and what comes of it: each HTTP request it makes to one, each connection it
// opens or fails to open, each server a listing could or could not reach, and
// each tool call. Header values are logged as they are sent, so a header a
// server needs and does not get shows as missing or empty, but for those that
// carry credentials, whose values are never logged.

// secretHeaders are the headers, by lowercase name, whose values are
// credentials: a log shows them redacted.
var secretHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"x-api-key":           true,
	"apikey":              true,
	"api-key":             true,
}

// redacted stands in a log for a credential's value.
const redacted = "[redacted]"

// maxLoggedRPCBody bounds how much of a request's body is read for its
// JSON-RPC method.
const maxLoggedRPCBody = 1 << 20

// loggedHeaders is headers as a log shows them: by lowercase name, the
// credentials redacted.
func loggedHeaders(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for name, values := range headers {
		name = strings.ToLower(name)
		value := strings.Join(values, ", ")
		if secretHeaders[name] {
			value = redacted
		}
		out[name] = value
	}
	return out
}

// loggedHeaders is the headers the connection sends on every request, as a log
// shows them: its configured ones, resolved for the run, and Authorization when
// a token source gives it a token.
func (c serverConn) loggedHeaders() map[string]string {
	headers := http.Header{}
	for name, value := range c.Headers {
		headers.Set(name, value)
	}
	if c.TokenSource != nil {
		headers.Set("Authorization", redacted)
	}
	return loggedHeaders(headers)
}

// logAttrs are what a log says of the connection: the server, the namespace
// owning its definition (none for a global one), its transport, where it is,
// and the headers it sends.
func (c serverConn) logAttrs() []any {
	attrs := []any{slog.String("server", c.Name)}
	if c.Namespace != "" {
		attrs = append(attrs, slog.String("owner_namespace", c.Namespace))
	}
	attrs = append(attrs, slog.String("transport", c.transportName()))
	if c.isStdio() {
		if len(c.Command) > 0 {
			attrs = append(attrs, slog.String("command", c.Command[0]))
		}
		return attrs
	}
	if u, err := url.Parse(c.Endpoint); err == nil {
		attrs = append(attrs, slog.String("endpoint", loggedURL(u)))
	}
	return append(attrs, slog.Any("headers", c.loggedHeaders()))
}

// transportName is the transport the connection speaks: SSE when none is
// named, as newClientTransport takes it.
func (c serverConn) transportName() string {
	if c.Transport == "" {
		return TransportSSE
	}
	return c.Transport
}

// loggedURL is u without its user information, query or fragment, which may
// carry credentials or a session's identifiers.
func loggedURL(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// rpcMethod is the JSON-RPC method a request's body names, read from a copy of
// the body, or empty when it names none, as a stream's GET does not.
func rpcMethod(req *http.Request) string {
	if req.Method != http.MethodPost || req.GetBody == nil {
		return ""
	}
	body, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer body.Close()
	var message struct {
		Method string `json:"method"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxLoggedRPCBody)).Decode(&message); err != nil {
		return ""
	}
	return message.Method
}

// logRequest logs one HTTP request to an MCP server, with the headers it went
// with, and what came back.
func logRequest(ctx context.Context, server string, req *http.Request, method string, resp *http.Response, err error, attrs ...any) {
	attrs = append([]any{
		slog.String("server", server),
		slog.String("http_method", req.Method),
		slog.String("url", loggedURL(req.URL)),
		slog.String("rpc_method", method),
		slog.Any("headers", loggedHeaders(req.Header)),
	}, attrs...)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "MCP request failed", append(attrs, slog.Any("error", err))...)
	case resp.StatusCode >= http.StatusBadRequest:
		slog.WarnContext(ctx, "MCP request refused", append(attrs, slog.Int("status", resp.StatusCode))...)
	default:
		slog.InfoContext(ctx, "MCP request", append(attrs, slog.Int("status", resp.StatusCode))...)
	}
}
