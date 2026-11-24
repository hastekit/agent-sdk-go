package sdk

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
)

// Skill is one folder of instructions the agent can pull in on demand — see
// agents.Skill.
type Skill = agents.Skill

// SkillClient lists an agent's global and namespace skills — see skills.Client.
type SkillClient = agents.SkillClient
type SkillSelection = agents.SkillSelection
type ListedSkill = agents.ListedSkill

// SkillStore lists and reads skills by namespace — see skills.Store. A skill
// client reads one as its built-in store and one as its user store.
type SkillStore = skills.Store

// NewSkillClient reads global skills from a built-in store, such as a folder,
// and each namespace's own from the store WithUserSkillStore gives, such as
// one of users' uploads.
var NewSkillClient = skills.NewClient

// WithUserSkillStore reads each namespace's own skills from a store.
var WithUserSkillStore = skills.WithUserStore

// NewDirSkillStore serves a folder of skills, rediscovered on every listing.
var NewDirSkillStore = skills.NewDirStore

// NewFSSkillStore supports embed.FS and other io/fs implementations.
var NewFSSkillStore = skills.NewFSStore

// FilterSkillStore lists only the named skills of a store.
var FilterSkillStore = skills.Filter
