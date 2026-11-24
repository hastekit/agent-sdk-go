// Dynamic agents: agents as data, created per namespace at run time.
//
//	OPENAI_API_KEY=... go run ./examples/agents/16_dynamic_agents
//	OPENAI_API_KEY=... go run ./examples/agents/16_dynamic_agents -runtime temporal
//	OPENAI_API_KEY=... go run ./examples/agents/16_dynamic_agents -runtime restate
//
// See README.md for the requests that create and run an agent.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/agui/web"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
)

type CurrentTimeInput struct {
	Zone string `json:"zone" jsonschema_description:"IANA time zone, such as Europe/Paris"`
}

type CurrentTimeOutput struct {
	Time string `json:"time"`
}

func CurrentTime(_ context.Context, in CurrentTimeInput) (CurrentTimeOutput, error) {
	location, err := time.LoadLocation(in.Zone)
	if err != nil {
		location = time.UTC
	}
	return CurrentTimeOutput{Time: time.Now().In(location).Format(time.RFC1123)}, nil
}

func main() {
	runtimeName := flag.String("runtime", "local", "local, temporal or restate")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client := hastekit.NewLLMClient([]hastekit.ProviderConfig{{
		ProviderName: hastekit.ProviderOpenAI,
		ApiKeys:      []*hastekit.APIKeyConfig{{Name: "default", APIKey: os.Getenv("OPENAI_API_KEY")}},
	}})

	// Built-in agents: every namespace can run them, none can replace them.
	builtIns := []agentconfig.Config{{
		Name:        "concierge",
		Description: "Answers general questions.",
		Model:       "OpenAI/gpt-4.1-mini",
		Instruction: "You are a friendly concierge.",
		Tools:       []string{"current_time"},
	}}
	// Each namespace's own agents, written over HTTP at run time.
	userAgents, err := agentconfig.NewFileStore("./data/agents")
	if err != nil {
		log.Fatal(err)
	}
	// Built-in skills a config may pick from; ./skills is optional.
	var builtInSkills skills.Store
	if dir, err := skills.NewDirStore("./skills"); err == nil {
		builtInSkills = dir
	}
	history, err := hastekit.OpenFileHistory("./data/conversations")
	if err != nil {
		log.Fatal(err)
	}
	defer history.Close()

	var runtime agentconfig.Runtime
	var serve func(context.Context, *agentconfig.Builder) error
	switch *runtimeName {
	case "local":
	case "temporal":
		broker, err := hastekit.NewRedisStreamBroker("localhost:6379", "", 0)
		if err != nil {
			log.Fatal(err)
		}
		rt, err := hastekit.NewTemporalRuntime("localhost:7233", broker)
		if err != nil {
			log.Fatal(err)
		}
		defer rt.Close()
		runtime = rt
		serve = func(ctx context.Context, b *agentconfig.Builder) error {
			if err := rt.RegisterBuilder(b); err != nil {
				return err
			}
			return rt.Serve(ctx)
		}
	case "restate":
		broker, err := hastekit.NewRedisStreamBroker("localhost:6379", "", 0)
		if err != nil {
			log.Fatal(err)
		}
		rt, err := hastekit.NewRestateRuntime("http://localhost:8080", broker)
		if err != nil {
			log.Fatal(err)
		}
		defer rt.Close()
		runtime = rt
		// Register http://localhost:9080 with the Restate admin API once.
		serve = func(ctx context.Context, b *agentconfig.Builder) error {
			if err := rt.RegisterBuilder(b); err != nil {
				return err
			}
			return rt.Serve(ctx, ":9080")
		}
	default:
		log.Fatalf("unknown runtime %q", *runtimeName)
	}

	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{
		// The allowlist: a config naming any other model is refused.
		Models: agentconfig.AllowModels(func(id string) (llm.Provider, error) {
			return client.Model(id), nil
		}, "OpenAI/gpt-4.1-mini", "OpenAI/gpt-4o"),
		BuiltInSkillStore: builtInSkills,
		Tools: map[string]agents.Tool{
			"current_time": hastekit.NewTool(CurrentTime, hastekit.WithName("current_time"), hastekit.WithDescription("The current time in a time zone.")),
		},
		History: history,
		Runtime: runtime,
	})
	if err != nil {
		log.Fatal(err)
	}

	if serve != nil {
		// The worker registers one generic workflow; agents written later
		// run through it with nothing more to register.
		go func() {
			if err := serve(ctx, builder); err != nil {
				log.Print(err)
				stop()
			}
		}()
	}

	// The registry finds built-ins and each namespace's own agents and hands
	// each run's snapshot to the builder; the worker above needs only the
	// builder.
	registry, err := agentconfig.NewRegistry(builder, builtIns, userAgents)
	if err != nil {
		log.Fatal(err)
	}
	handler := web.Handler(registry,
		agui.WithAgentConfigs(),
		// Demo only: trust a header. Derive the namespace from authentication.
		agui.WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("X-User"), nil }),
	)
	server := &http.Server{Addr: ":8070", Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("serving on http://localhost:8070 (%s runtime)", *runtimeName)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
