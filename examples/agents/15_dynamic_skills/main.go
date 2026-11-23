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

	// Users' own skills, shared by every agent that uses this store. Its empty
	// namespace could hold global skills too; this example keeps them elsewhere.
	store, err := skills.NewFileStore(*storeDir)
	if err != nil {
		log.Fatal(err)
	}

	// Developer-owned global skills: always on, never shadowed by a user's.
	builtins, err := skills.NewDirStore("./samples/skills/skills")
	if err != nil {
		log.Fatal(err)
	}
	// Implement teamSkills with a database or HTTP-backed store.
	// List runs at the start of each run, so new skills appear without rebuilding the agent.
	team := &teamSkills{}

	client := hastekit.NewLLMClient([]hastekit.ProviderConfig{{ProviderName: hastekit.ProviderOpenAI, ApiKeys: []*hastekit.APIKeyConfig{{APIKey: os.Getenv("OPENAI_API_KEY")}}}})
	agent, err := hastekit.NewAgent(&hastekit.AgentConfig{
		Name: "ReleaseAssistant", LLM: client.Model("OpenAI/gpt-4.1-mini"),
		Instruction: hastekit.NewPrompt("Help prepare releases. Read relevant skills before answering.", prompts.WithResolver(prompts.DefaultResolvers()...)),
		SkillClient: skills.NewClient(store, builtins, team),
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

// teamSkills is a custom read-only store; any skills.Store can back an agent's skills.
// It serves global skills, so it answers only for the empty namespace.
type teamSkills struct{}

var teamBundles = map[string]skills.Bundle{
	"release-review": {Files: map[string][]byte{
		"SKILL.md":     []byte("---\nname: release-review\ndescription: Review a release before shipping.\n---\nReview the release using checklist.md. Read it with read_skill before advising the user.\n"),
		"checklist.md": []byte("Check tests, upgrade notes, rollback steps, and release ownership.\n"),
	}},
	"writing": {Files: map[string][]byte{
		"SKILL.md": []byte("---\nname: writing\ndescription: Write concise release notes.\n---\nUse short sentences. Explain user-visible changes and required migration steps.\n"),
	}},
}

func (s *teamSkills) List(ctx context.Context, namespace string, _ skills.ListOptions) (skills.Page, error) {
	page := skills.Page{Skills: []skills.Metadata{}}
	if namespace == "" {
		page.Skills = append(page.Skills,
			skills.Metadata{Name: "release-review", Description: "Review a release before shipping.", Resources: []string{"checklist.md"}},
			skills.Metadata{Name: "writing", Description: "Write concise release notes."},
		)
	}
	return page, ctx.Err()
}

func (s *teamSkills) Get(ctx context.Context, namespace, name string) (skills.Bundle, error) {
	bundle, ok := teamBundles[name]
	if namespace != "" || !ok {
		return skills.Bundle{}, skills.ErrNotFound
	}
	return bundle, ctx.Err()
}
