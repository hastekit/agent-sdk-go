package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestOAuthConfigEditsReplacePooledSessions(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*OAuthConfig)
	}{
		{"client ID", func(c *OAuthConfig) { c.ClientID = "replacement-client" }},
		{"token URL", func(c *OAuthConfig) { c.TokenURL = "https://replacement.example/token" }},
		{"client secret", func(c *OAuthConfig) { c.ClientSecret = "replacement-secret" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "oauth-edit", Version: "1"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: "read"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: req.Extra.Header.Get("Authorization")}}}, nil, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
			endpoint := httptest.NewServer(handler)
			defer endpoint.Close()
			defer endpoint.CloseClientConnections()

			provider := authorizationProvider(t)
			store := NewMemoryStore()
			config := catalogConfig("oauth-edit", endpoint.URL)
			config.Authorization = oauthTestConfig("https://accounts.example/token")
			config.DisableStandaloneSSE = true
			saveGrant := func(token string) {
				t.Helper()
				key := OAuthCredentialKey(Connector{Name: config.Name, Authorization: config.Authorization})
				require.NoError(t, provider.store.Save(t.Context(), "alice", key, &oauth2.Token{AccessToken: token}))
				require.NoError(t, store.Put(t.Context(), "", config))
			}
			saveGrant("old-account")
			client := NewClient(store).WithCredentials(provider)
			_, tools, err := client.ListTools(t.Context(), "alice", nil)
			require.NoError(t, err)
			require.Len(t, tools, 1)
			original := tools[0]
			call := &agents.ToolCall{Namespace: "alice", FunctionCallMessage: &responses.FunctionCallMessage{Name: "oauth-edit__read", Arguments: "{}"}}
			execute := func(tool agents.Tool, wantToken string) *mcp.ClientSession {
				t.Helper()
				result, err := tool.Execute(t.Context(), call)
				require.NoError(t, err)
				require.Equal(t, "Bearer "+wantToken, *result.Output.OfString)
				current, err := client.build(t.Context(), config)
				require.NoError(t, err)
				conn, err := current.connFor(t.Context(), "alice", nil)
				require.NoError(t, err)
				session, release, err := globalPool.Checkout(t.Context(), conn)
				require.NoError(t, err)
				release()
				return session
			}
			originalSession := execute(original, "old-account")

			// Replacing a stored access token keeps the live session and reads the new token.
			saveGrant("rotated-token")
			require.Same(t, originalSession, execute(original, "rotated-token"))

			// Configuration changes must replace the captured OAuth settings, even on old tools.
			test.edit(config.Authorization)
			saveGrant("new-account")
			currentSession := execute(original, "new-account")
			require.NotSame(t, originalSession, currentSession)
			_, tools, err = client.ListTools(t.Context(), "alice", nil)
			require.NoError(t, err)
			require.Len(t, tools, 1)
			require.Same(t, currentSession, execute(tools[0], "new-account"))
		})
	}
}

func TestFilterEditsRevokePreviouslyListedTools(t *testing.T) {
	for _, test := range []struct {
		name   string
		filter ToolFilter
	}{
		{"exclude", ToolFilter{Exclude: []string{"write"}}},
		{"narrow include", ToolFilter{Include: []string{"read"}}},
		{"exclude wins", ToolFilter{Include: []string{"write"}, Exclude: []string{"write"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemoryStore()
			config := catalogConfig("filter-edit", catalogServer(t, "executed-write"))
			require.NoError(t, store.Put(t.Context(), "", config))
			client := NewClient(store)
			_, tools, err := client.ListTools(t.Context(), "alice", nil)
			require.NoError(t, err)
			var write agents.Tool
			for _, tool := range tools {
				if tool.GetToolDescriptor().Name == "write" {
					write = tool
				}
			}
			require.NotNil(t, write)

			// Durable runtimes retain only a serialized descriptor from discovery.
			data, err := json.Marshal(write.GetToolDescriptor())
			require.NoError(t, err)
			var descriptor agents.BaseTool
			require.NoError(t, json.Unmarshal(data, &descriptor))
			call := &agents.ToolCall{Namespace: "alice", FunctionCallMessage: &responses.FunctionCallMessage{Name: "filter-edit__write", Arguments: "{}"}}
			config.ToolFilter = test.filter
			require.NoError(t, store.Put(t.Context(), "", config))
			_, err = write.Execute(t.Context(), call)
			require.ErrorContains(t, err, "excluded by the current filter")
			_, err = client.CallTool(t.Context(), &descriptor, call)
			require.ErrorContains(t, err, "excluded by the current filter")

			// Restoring the original tool name permits calls without another discovery.
			config.ToolFilter = ToolFilter{Include: []string{"write"}}
			require.NoError(t, store.Put(t.Context(), "", config))
			result, err := write.Execute(t.Context(), call)
			require.NoError(t, err)
			require.Equal(t, "executed-write", *result.Output.OfString)
		})
	}
}
