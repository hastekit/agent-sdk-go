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

// SkillStore lists and reads skills by namespace; its empty namespace holds
// global (developer-owned) skills — see skills.Store.
type SkillStore = skills.Store

// NewSkillClient reads skills from one or more stores, such as a store of
// users' uploads and a folder of built-in skills.
var NewSkillClient = skills.NewClient

// NewDirSkillStore serves a folder of skills as global skills, rediscovered on every listing.
var NewDirSkillStore = skills.NewDirStore

// NewFSSkillStore supports embed.FS and other io/fs implementations.
var NewFSSkillStore = skills.NewFSStore

// FilterSkillStore lists only the named skills of a store, global or users' own.
var FilterSkillStore = skills.Filter
