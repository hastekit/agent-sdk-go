// Run from the repository root with OPENAI_API_KEY set. Add -serve to try
// the embedded UI at http://localhost:8080 instead of running one turn.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/agui/web"
	"github.com/hastekit/agent-sdk-go/pkg/skills"
)

func main() {
	serve := flag.Bool("serve", false, "serve the embedded UI")
	storeDir := flag.String("skill-store", "./data/skills", "directory for uploaded skills")
	flag.Parse()
	store, err := skills.NewFileStore(*storeDir)
	if err != nil {
		log.Fatal(err)
	}
	library, err := skills.NewSkillSet("library", store)
	if err != nil {
		log.Fatal(err)
	}
	builtins, err := hastekit.NewFilesystemSkillSet("builtin", "./samples/skills/skills")
	if err != nil {
		log.Fatal(err)
	}
	// Replace these callbacks with a database or HTTP-backed catalog and reader.
	// List runs at the start of each run, so newly saved skills appear without
	// rebuilding the agent. Policies come from the host, never the model.
	team := hastekit.SkillSetFuncs{
		Name: "team",
		List: func(ctx context.Context, namespace string, rc map[string]any) ([]hastekit.Skill, error) {
			return []hastekit.Skill{
				{Name: "release-review", Description: "Review a release before shipping.", Policy: hastekit.SkillOptIn, Resources: []string{"checklist.md"}},
				{Name: "writing", Description: "Write concise release notes.", Policy: hastekit.SkillEnabled},
				{Name: "retired", Description: "Retired release process.", Policy: hastekit.SkillBlocked},
			}, ctx.Err()
		},
		Resolve: func(ctx context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			switch {
			case name == "release-review" && (file == "" || file == "SKILL.md"):
				return "Review the release using checklist.md. Read it with read_skill before advising the user.", nil
			case name == "release-review" && file == "checklist.md":
				return "Check tests, upgrade notes, rollback steps, and release ownership.", nil
			case name == "writing" && (file == "" || file == "SKILL.md"):
				return "Use short sentences. Explain user-visible changes and required migration steps.", nil
			default:
				return "", fmt.Errorf("unknown skill or resource: %s/%s", name, file)
			}
		},
	}
	client := hastekit.NewLLMClient([]hastekit.ProviderConfig{{ProviderName: hastekit.ProviderOpenAI, ApiKeys: []*hastekit.APIKeyConfig{{APIKey: os.Getenv("OPENAI_API_KEY")}}}})
	agent, err := hastekit.NewAgent(&hastekit.AgentConfig{
		Name: "ReleaseAssistant", LLM: client.Model("OpenAI/gpt-4.1-mini"),
		Instruction: hastekit.NewPrompt("Help prepare releases. Read relevant skills before answering.", prompts.WithResolver(prompts.DefaultResolvers()...)),
		Skills:      []hastekit.SkillSet{builtins, team, library},
	})
	if err != nil {
		log.Fatal(err)
	}
	if *serve {
		registry := hastekit.NewRegistry()
		if err := registry.Register(agent); err != nil {
			log.Fatal(err)
		}
		log.Fatal(web.Serve(":8080", registry, agui.WithSkillStore(store)))
	}
	selection := hastekit.SkillSelection{Enable: []string{"release-review"}, Disable: []string{"writing"}}
	catalog, err := agent.ListSkills(context.Background(), "default", nil, selection)
	if err != nil {
		log.Fatal(err)
	}
	for _, skill := range catalog {
		fmt.Printf("%s: enabled=%t policy=%s\n", skill.Name, skill.Enabled, skill.Policy)
	}
	result, err := agent.Run(context.Background(), &hastekit.Input{
		Skills: selection, Message: hastekit.UserTurn("What should we check before shipping a release?"),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Text())
}
