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

// SkillSource is a fixed set of developer-owned (global) skills.
type SkillSource = skills.Source

// NewSkillClient reads users' own skills from a store; add globals with WithGlobalSkills.
var NewSkillClient = skills.NewClient

// NewDirSkillSource discovers global skills from a folder at run time.
var NewDirSkillSource = skills.NewDirSource

// NewFSSkillSource supports embed.FS and other io/fs implementations.
var NewFSSkillSource = skills.NewFSSource
