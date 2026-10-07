// A deterministic Hastekit agent server for the Google ADK interoperability demo.
// Only the model is substituted; agent execution, history, streaming and A2A
// transport use the real SDK implementations. No provider credentials are needed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type registry map[string]*agents.Agent

func (r registry) Agent(name string) (*agents.Agent, bool) { a, ok := r[name]; return a, ok }
func (r registry) AgentNames() []string                    { return []string{"Echo", "Report"} }

type demoModel struct{ report bool }

func (m demoModel) NewStreamingResponses(ctx context.Context, _ *agents.ModelCall, in *responses.Request, cb func(*responses.ResponseChunk)) (*responses.Response, error) {
	var turns []string
	for _, msg := range in.Input.OfInputMessageList {
		if msg.OfEasyInput != nil && msg.OfEasyInput.Role == constants.RoleUser && msg.OfEasyInput.Content.OfString != nil {
			turns = append(turns, *msg.OfEasyInput.Content.OfString)
		}
		if msg.OfInputMessage != nil && msg.OfInputMessage.Role == constants.RoleUser {
			var text strings.Builder
			for _, part := range msg.OfInputMessage.Content {
				if part.OfInputText != nil {
					text.WriteString(part.OfInputText.Text)
				}
			}
			turns = append(turns, text.String())
		}
	}
	if len(turns) == 0 {
		return nil, errors.New("no user message")
	}
	prompt := turns[len(turns)-1]
	if prompt == "fail" {
		return nil, errors.New("intentional demo model failure")
	}
	text := "Hastekit says: " + prompt
	if prompt == "history" {
		text = strings.Join(turns, " | ")
	}
	if m.report {
		data, err := json.Marshal(map[string]any{"agent": "hastekit", "request": prompt, "status": "ok", "items": []string{"text replies", "JSON artifacts"}})
		if err != nil {
			return nil, err
		}
		text = string(data)
	}
	// Multiple deltas exercise append and final replacement across the wire.
	runes := []rune(text)
	for start := 0; start < len(runes); start += 8 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+8, len(runes))
		cb(&responses.ResponseChunk{OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{Delta: string(runes[start:end])}})
	}
	return &responses.Response{Output: []responses.OutputMessageUnion{{OfOutputMessage: &responses.OutputMessage{
		ID: responses.NewOutputItemMessageID(), Role: constants.RoleAssistant,
		Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: text}}},
	}}}}, nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8087", "listen address; use port 0 to allocate a free port")
	flag.Parse()
	r := registry{
		"Echo":   agents.NewAgent(&agents.AgentOptions{Name: "Echo"}).WithLLM(demoModel{}),
		"Report": agents.NewAgent(&agents.AgentOptions{Name: "Report", Output: map[string]any{"type": "object"}}).WithLLM(demoModel{report: true}),
	}
	mux := http.NewServeMux()
	mux.Handle("/api/agui/", http.StripPrefix("/api/agui", agui.NewHandler(r)))
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	// The integration harness reads this one machine-readable startup line.
	fmt.Printf("LISTEN http://%s\n", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
