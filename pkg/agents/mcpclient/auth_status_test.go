package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The status a server refuses us with never reaches the caller: the SDK's http
// transports report a non-2xx by its status text alone. connect reads it off
// the response instead, so what comes back says whether the user can do
// anything about it.
func TestConnectClassifiesByHTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   agents.ToolsetErrorKind
		auth   bool
	}{
		{name: "401 wants credentials", status: http.StatusUnauthorized, want: agents.ToolsetErrorAuth, auth: true},
		{name: "403 finds them insufficient", status: http.StatusForbidden, want: agents.ToolsetErrorAuth, auth: true},
		{name: "500 is just a broken server", status: http.StatusInternalServerError, auth: false},
		{name: "404 is not an authorization problem", status: http.StatusNotFound, auth: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer realm="OAuth", error="invalid_token"`)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			_, err := connect(context.Background(), serverConn{
				Transport: TransportStreamableHTTP,
				Endpoint:  srv.URL,
			})
			require.Error(t, err)

			var te *agents.ToolsetError
			if !tc.auth {
				assert.False(t, errors.As(err, &te), "only an authorization status should be classified as one")
				return
			}

			require.True(t, errors.As(err, &te), "a %d must come back classified", tc.status)
			assert.Equal(t, tc.want, te.Kind)
			assert.ErrorIs(t, err, te.Unwrap(), "the transport's own error stays reachable underneath")
		})
	}
}

// A status the server answers with mid-conversation must not be mistaken for a
// refusal of the handshake.
func TestAuthStatusIgnoresOrdinaryStatuses(t *testing.T) {
	var a authStatus

	a.record(http.StatusOK)
	a.record(http.StatusAccepted)
	a.record(http.StatusInternalServerError)
	assert.False(t, a.refused())

	a.record(http.StatusForbidden)
	assert.True(t, a.refused())
}

func TestAuthStatusRefusedIsNilSafe(t *testing.T) {
	// stdio has no status to record — a child process is not an http server.
	var a *authStatus
	assert.False(t, a.refused())
}
