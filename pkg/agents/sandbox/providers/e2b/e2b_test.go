package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}
}
func TestNativeProtocols(t *testing.T) {
	calls := map[string]int{}
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Path]++
		if r.URL.Host == "api.test" {
			if r.Header.Get("X-API-Key") != "secret" {
				t.Fatal("missing control key")
			}
			if r.Method == "DELETE" {
				return response(nil), nil
			}
			return response([]byte(`{"sandboxID":"abc","envdAccessToken":"token","state":"running"}`)), nil
		}
		if r.Header.Get("X-API-Key") != "" || r.Header.Get("X-Access-Token") != "token" {
			t.Fatal("incorrect credential scope")
		}
		switch r.URL.Path {
		case "/process.Process/Start":
			b, _ := io.ReadAll(r.Body)
			var req struct {
				Process struct {
					Cmd  string
					Args []string
				}
				Stdin bool
			}
			if len(b) < 5 {
				t.Fatal("missing Connect envelope")
			}
			if err := json.Unmarshal(b[5:], &req); err != nil {
				t.Fatal(err)
			}
			if req.Process.Cmd != "printf" || len(req.Process.Args) != 2 || req.Process.Args[1] != "$(touch /bad)" {
				t.Fatalf("argv changed: %+v", req)
			}
			var body []byte
			for _, event := range []string{`{"event":{"start":{"pid":42}}}`, `{"event":{"data":{"stdout":"aGk="}}}`, `{"event":{"data":{"stderr":"ZXJy"}}}`, `{"event":{"end":{"exitCode":7,"exited":true}}}`} {
				body = append(body, frame([]byte(event))...)
			}
			trailer := frame([]byte(`{}`))
			trailer[0] = 2
			body = append(body, trailer...)
			return response(body), nil
		default:
			t.Fatalf("unexpected request %s", r.URL)
			return nil, nil
		}
	})}
	p, err := New(Config{APIKey: "secret", BaseURL: "https://api.test", HTTPClient: client, Profiles: map[string]Profile{"base": {TemplateID: "template"}}, EnvdURL: func(string, string) string { return "https://envd.test" }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := p.Create(ctx, sandbox.CreateRequest{Profile: "base"})
	if err != nil {
		t.Fatal(err)
	}
	s, err = p.Connect(ctx, s.Reference())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"printf", "%s", "$(touch /bad)"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "hi" || result.Stderr != "err" || result.ExitCode != 7 {
		t.Fatalf("%+v", result)
	}
	if err := p.Delete(ctx, s.Reference()); err != nil {
		t.Fatal(err)
	}
}
func TestInterruptedStreamKillsProcess(t *testing.T) {
	killed := false
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "SendSignal") {
			killed = true
			return response([]byte(`{}`)), nil
		}
		return response(frame([]byte(`{"event":{"start":{"pid":42}}}`))), nil
	})}
	p, _ := New(Config{APIKey: "x", HTTPClient: client, EnvdURL: func(string, string) string { return "https://envd.test" }})
	s, _ := p.wrap(info{ID: "abc"}, time.Now().Add(time.Hour))
	_, err := s.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sleep", "5"}})
	if !errors.Is(err, io.EOF) || !killed {
		t.Fatalf("err=%v killed=%v", err, killed)
	}
}

func status(code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}
}

// A sandbox that dies on timeout takes the session's files with it, and
// Connect cannot bring it back.
func TestCreatePausesInsteadOfDying(t *testing.T) {
	var body map[string]any
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/v2/sandboxes" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return response([]byte(`{"sandboxID":"abc","envdAccessToken":"token"}`)), nil
	})}
	p, _ := New(Config{APIKey: "x", BaseURL: "https://api.test", HTTPClient: client, Profiles: map[string]Profile{"base": {TemplateID: "template", TTL: 90 * time.Second}}})
	if _, err := p.Create(context.Background(), sandbox.CreateRequest{Profile: "base", Session: sandbox.SessionKey{Namespace: "ns", SessionID: "s1"}}); err != nil {
		t.Fatal(err)
	}
	metadata, _ := body["metadata"].(map[string]any)
	if body["autoPause"] != true || body["timeout"] != float64(90) {
		t.Fatalf("lifecycle %v", body)
	}
	if metadata["ttl"] != "90" || metadata["managed_by"] != "hastekit" || metadata["session"] != "s1" {
		t.Fatalf("metadata %v", metadata)
	}
}

// Reconnecting is what brings a paused sandbox back, and it gets the lifetime
// it was created with rather than E2B's default.
func TestConnectResumesForTheTTLItWasCreatedWith(t *testing.T) {
	for metadata, want := range map[string]float64{
		`{"ttl":"900"}`:  900,
		`{}`:             300,
		`{"ttl":"junk"}`: 300,
	} {
		var timeout float64
		client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			switch r.Method + " " + r.URL.Path {
			case "GET /sandboxes/abc":
				return response([]byte(`{"sandboxID":"abc","state":"paused","metadata":` + metadata + `}`)), nil
			case "POST /v2/sandboxes/abc/connect":
				var body struct {
					Timeout float64 `json:"timeout"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				timeout = body.Timeout
				return response([]byte(`{"sandboxID":"abc","envdAccessToken":"token"}`)), nil
			}
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
			return nil, nil
		})}
		p, _ := New(Config{APIKey: "x", BaseURL: "https://api.test", HTTPClient: client})
		s, err := p.Connect(context.Background(), sandbox.Reference{Provider: "e2b", ID: "abc"})
		if err != nil {
			t.Fatal(err)
		}
		if s.Reference().ID != "abc" || timeout != want {
			t.Fatalf("metadata %s: ref %v, timeout %v, want %v", metadata, s.Reference(), timeout, want)
		}
	}
}

func TestConnectNeverReplacesAMissingSandbox(t *testing.T) {
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		return status(404), nil
	})}
	p, _ := New(Config{APIKey: "x", BaseURL: "https://api.test", HTTPClient: client})
	if _, err := p.Connect(context.Background(), sandbox.Reference{Provider: "e2b", ID: "abc"}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

// A pause in the middle of a command would cut the stream reporting its exit.
func TestLongCommandKeepsTheSandboxRunning(t *testing.T) {
	var calls []string
	var extended float64
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v2/sandboxes/abc/connect":
			calls = append(calls, "extend")
			var body struct {
				Timeout float64 `json:"timeout"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			extended = body.Timeout
			return response([]byte(`{"sandboxID":"abc"}`)), nil
		case "/process.Process/Start":
			calls = append(calls, "start")
			var body []byte
			for _, event := range []string{`{"event":{"start":{"pid":42}}}`, `{"event":{"end":{"exitCode":0,"exited":true}}}`} {
				body = append(body, frame([]byte(event))...)
			}
			trailer := frame([]byte(`{}`))
			trailer[0] = 2
			return response(append(body, trailer...)), nil
		}
		t.Fatalf("unexpected request %s", r.URL)
		return nil, nil
	})}
	p, _ := New(Config{APIKey: "x", BaseURL: "https://api.test", HTTPClient: client, EnvdURL: func(string, string) string { return "https://envd.test" }})
	s, _ := p.wrap(info{ID: "abc"}, time.Now().Add(time.Minute))
	ctx := context.Background()
	if _, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sleep", "1"}, Timeout: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "extend,start" || extended != 630 {
		t.Fatalf("calls %v, extended to %v", calls, extended)
	}
	if _, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "extend,start,start" {
		t.Fatalf("a command that fits extended again: %v", calls)
	}
}

func TestDeletePausedOnlyDeletesSandboxesIdleLongEnough(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	var deleted []string
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v2/sandboxes":
			q := r.URL.Query()
			if q.Get("state") != "paused" || q.Get("metadata") != "managed_by=hastekit" {
				t.Fatalf("query %s", r.URL.RawQuery)
			}
			switch q.Get("nextToken") {
			case "":
				page := response([]byte(`[{"sandboxID":"old","endAt":"` + old + `"},{"sandboxID":"recent","endAt":"` + recent + `"}]`))
				page.Header.Set("X-Next-Token", "page2")
				return page, nil
			case "page2":
				return response([]byte(`[{"sandboxID":"older","endAt":"` + old + `"},{"sandboxID":"undated"}]`)), nil
			}
		case r.Method == "DELETE":
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/sandboxes/"))
			return response(nil), nil
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		return nil, nil
	})}
	p, _ := New(Config{APIKey: "x", BaseURL: "https://api.test", HTTPClient: client})
	n, err := p.DeletePaused(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || strings.Join(deleted, ",") != "old,older" {
		t.Fatalf("deleted %d: %v", n, deleted)
	}
}
