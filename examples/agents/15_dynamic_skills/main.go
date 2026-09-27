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
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/agui/web"
)

func main() {
	serve := flag.Bool("serve", false, "serve the embedded UI")
	storeDir := flag.String("skill-store", "./data/skills", "directory for users' uploaded skills")
	flag.Parse()

	// Users' own skills, shared by every agent that uses this store.
	store, err := skills.NewFileStore(*storeDir)
	if err != nil {
		log.Fatal(err)
	}
	userSkills := skills.NewClient(store)

	// Developer-owned global skills for this agent: always on, never shadowed by a user's.
	builtins, err := skills.NewDirSource("./samples/skills/skills")
	if err != nil {
		log.Fatal(err)
	}
	// Implement teamSkills with a database or HTTP-backed catalog and reader.
	// List runs at the start of each run, so new skills appear without rebuilding the agent.
	team := &teamSkills{}

	client := hastekit.NewLLMClient([]hastekit.ProviderConfig{{ProviderName: hastekit.ProviderOpenAI, ApiKeys: []*hastekit.APIKeyConfig{{APIKey: os.Getenv("OPENAI_API_KEY")}}}})
	agent, err := hastekit.NewAgent(&hastekit.AgentConfig{
		Name: "ReleaseAssistant", LLM: client.Model("OpenAI/gpt-4.1-mini"),
		Instruction: hastekit.NewPrompt("Help prepare releases. Read relevant skills before answering.", prompts.WithResolver(prompts.DefaultResolvers()...)),
		SkillClient: userSkills.WithGlobalSkills(builtins, team),
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

	// A run can turn off the user's own skills; naming a global here has no effect.
	selection := hastekit.SkillSelection{Disable: []string{"my-drafts"}}
	catalog, err := agent.ListSkills(context.Background(), "default", nil, selection)
	if err != nil {
		log.Fatal(err)
	}
	for _, skill := range catalog {
		fmt.Printf("%s: enabled=%t global=%t\n", skill.Name, skill.Enabled, skill.Global)
	}
	result, err := agent.Run(context.Background(), &hastekit.Input{
		Namespace: "default",
		Skills:    selection, Message: hastekit.UserTurn("What should we check before shipping a release?"),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Text())
}

// teamSkills is a custom global source; any skills.Source can back an agent's globals.
type teamSkills struct{}

func (s *teamSkills) List(ctx context.Context) ([]hastekit.Skill, error) {
	return []hastekit.Skill{
		{Name: "release-review", Description: "Review a release before shipping.", Resources: []string{"checklist.md"}},
		{Name: "writing", Description: "Write concise release notes."},
	}, ctx.Err()
}

func (s *teamSkills) Read(ctx context.Context, name, file string) (string, error) {
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
}
