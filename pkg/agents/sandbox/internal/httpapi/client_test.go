package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

type conflictTransport struct{ calls int }

func (t *conflictTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader("provider conflict")), Header: http.Header{}}, nil
}
func TestConflictIsTypedWithoutRetry(t *testing.T) {
	transport := &conflictTransport{}
	client, err := New("https://provider.test", &http.Client{Transport: transport}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = client.JSON(context.Background(), "POST", "/sandbox", struct{}{}, nil)
	if !errors.Is(err, sandbox.ErrConflict) || transport.calls != 1 {
		t.Fatalf("calls=%d err=%v", transport.calls, err)
	}
}
