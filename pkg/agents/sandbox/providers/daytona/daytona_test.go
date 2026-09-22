package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}
}
func TestLifecycleExecAndBinaryFiles(t *testing.T) {
	payload := []byte{0, 255, 128, 10}
	deletes := 0
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing auth")
		}
		switch r.URL.Path {
		case "/api/sandbox":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["ttlMinutes"] != float64(2) || b["snapshot"] != "base-snapshot" {
				t.Fatal(b)
			}
			return response([]byte(`{"id":"abc","state":"started","toolboxProxyUrl":"https://toolbox.test/toolbox"}`)), nil
		case "/api/sandbox/abc":
			if r.Method == "DELETE" {
				deletes++
				return response(nil), nil
			}
			return response([]byte(`{"id":"abc","state":"started","toolboxProxyUrl":"https://toolbox.test/toolbox"}`)), nil
		case "/toolbox/abc/process/execute":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["command"] != `'printf' '%s' 'a'"'"'$(touch /bad)'` {
				t.Fatal(b)
			}
			return response([]byte(`{"exitCode":3,"result":"combined"}`)), nil
		case "/toolbox/abc/files/upload-v2":
			b, _ := io.ReadAll(r.Body)
			if !bytes.Equal(b, payload) || r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatal("invalid native upload")
			}
			return response(nil), nil
		case "/toolbox/abc/files/download":
			if r.URL.Query().Get("path") != "a b.bin" {
				t.Fatal(r.URL)
			}
			return response(payload), nil
		case "/toolbox/abc/files":
			if r.Method != "DELETE" {
				t.Fatal(r.Method)
			}
			return response(nil), nil
		default:
			t.Fatalf("unexpected request %s", r.URL)
			return nil, nil
		}
	})}
	p, err := New(Config{APIKey: "secret", BaseURL: "https://api.test/api", HTTPClient: client, Profiles: map[string]Profile{"base": {Snapshot: "base-snapshot"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := p.Create(ctx, sandbox.CreateRequest{Profile: "base", TTL: 61 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s, err = p.Connect(ctx, s.Reference())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"printf", "%s", "a'$(touch /bad)"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OutputCombined || result.Stdout != "combined" || result.ExitCode != 3 {
		t.Fatal(result)
	}
	if err := s.Files().Write(ctx, "a b.bin", bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Files().Read(ctx, "a b.bin")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(b, payload) {
		t.Fatal(b)
	}
	if err := s.Files().Remove(ctx, "a b.bin"); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, s.Reference()); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatal(deletes)
	}
}

func TestCreateConflictRecovery(t *testing.T) {
	for _, scenario := range []string{"matching", "different-request", "different-snapshot", "missing", "unauthorized", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			req := sandbox.CreateRequest{Profile: "base", Session: sandbox.SessionKey{Namespace: "ns", SessionID: "session"}}
			posts, gets, deletes := 0, 0, 0
			client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				switch r.Method {
				case "POST":
					posts++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body["name"] != "hastekit-"+sandbox.ResourceID(req.Session) {
						t.Fatal("missing stable name")
					}
					res := response([]byte(`{"error":"conflict"}`))
					res.StatusCode = 409
					if scenario == "unauthorized" {
						res.StatusCode = 401
					}
					return res, nil
				case "GET":
					gets++
					if r.URL.Path != "/sandbox/hastekit-"+sandbox.ResourceID(req.Session) {
						t.Fatal(r.URL.Path)
					}
					if scenario == "missing" {
						res := response(nil)
						res.StatusCode = 404
						return res, nil
					}
					value := info{ID: "existing", State: "started", Snapshot: "snapshot", ToolboxProxyURL: "https://toolbox.test", Labels: map[string]string{"hastekit.request": sandbox.RequestFingerprint(req)}}
					if scenario == "different-request" {
						value.Labels["hastekit.request"] = "other"
					}
					if scenario == "different-snapshot" {
						value.Snapshot = "other"
					}
					if scenario == "failed" {
						value.State = "error"
					}
					data, _ := json.Marshal(value)
					return response(data), nil
				case "DELETE":
					deletes++
					return response(nil), nil
				}
				t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				return nil, nil
			})}
			provider, err := New(Config{APIKey: "key", BaseURL: "https://control.test", HTTPClient: client, Profiles: map[string]Profile{"base": {Snapshot: "snapshot"}}})
			if err != nil {
				t.Fatal(err)
			}
			sb, err := provider.Create(context.Background(), req)
			if scenario == "matching" {
				if err != nil || sb.Reference().ID != "existing" {
					t.Fatalf("%v %v", sb, err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if scenario == "different-request" || scenario == "different-snapshot" || scenario == "missing" {
				if !errors.Is(err, sandbox.ErrConflict) {
					t.Fatalf("lost conflict: %v", err)
				}
			}
			if posts != 1 || deletes != 0 {
				t.Fatalf("posts=%d deletes=%d", posts, deletes)
			}
			if scenario == "unauthorized" && gets != 0 {
				t.Fatal("lookup after authorization failure")
			}
		})
	}
}
