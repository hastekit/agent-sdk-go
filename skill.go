package sdk

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Skill is one folder of instructions the agent can pull in on demand — see
// agents.Skill.
type Skill = agents.Skill

// SkillProvider is where an agent's skills come from. Set one on
// AgentConfig.Skills and the agent does the rest: it lists the skills in its
// prompt and adds the provider's reader tool to its own tools.
type SkillProvider = agents.SkillProvider

// SkillRegistry holds the skills read out of one or more folders — see
// agents.SkillRegistry. It is a SkillProvider.
type SkillRegistry = agents.SkillRegistry

// SkillList is a SkillProvider for skills the agent can already reach by other
// means, such as ones staged into its sandbox. It lists them and adds no tool,
// and says nothing about what they are or how to read them — use
// SkillsWithHint to say.
type SkillList = agents.SkillList

// SkillsWithHint is a SkillProvider for skills your own host serves: it lists
// them, adds no tool, and introduces them to the model in your words.
type SkillsWithHint = agents.SkillsWithHint

// NewSkillRegistryFromDir loads every skill in a directory on disk:
//
//	registry, err := hastekit.NewSkillRegistryFromDir("./skills")
//
//	agent := hastekit.NewAgent(&hastekit.AgentConfig{
//		Instruction: hastekit.NewPrompt("..."),
//		Skills:      registry,
//	})
var NewSkillRegistryFromDir = agents.NewSkillRegistryFromDir

// NewSkillRegistry loads every skill in the given filesystems — an embed.FS,
// so the skills ship inside the binary, or anything else that implements
// fs.FS:
//
//	//go:embed skills
//	var skillsFS embed.FS
//
//	registry, err := hastekit.NewSkillRegistry(skillsFS)
var NewSkillRegistry = agents.NewSkillRegistry
