package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}
}
func TestNativeProtocols(t *testing.T) {
	payload := []byte{0, 255, 128, '\n'}
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
		case "/files":
			if r.URL.Query().Get("path") != "/home/user/a b.bin" {
				t.Fatal(r.URL)
			}
			if r.Method == "GET" {
				return response(payload), nil
			}
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(part)
			if !bytes.Equal(b, payload) {
				t.Fatalf("corrupted upload %v", b)
			}
			return response([]byte(`[]`)), nil
		case "/filesystem.Filesystem/Remove":
			return response([]byte(`{}`)), nil
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
	path := "/home/user/a b.bin"
	if err := s.Files().Write(ctx, path, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Files().Read(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(b, payload) {
		t.Fatal(b)
	}
	if err := s.Files().Remove(ctx, path); err != nil {
		t.Fatal(err)
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
	s, _ := p.wrap(info{ID: "abc"})
	_, err := s.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sleep", "5"}})
	if !errors.Is(err, io.EOF) || !killed {
		t.Fatalf("err=%v killed=%v", err, killed)
	}
}
