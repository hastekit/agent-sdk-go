package sdk

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
)

// Skill is one folder of instructions the agent can pull in on demand — see
// agents.Skill.
type Skill = agents.Skill

// SkillSet provides a runtime catalog and resolver for a group of skills.
type SkillSet = agents.SkillSet
type SkillSelection = agents.SkillSelection
type ListedSkill = agents.ListedSkill

// FilesystemSkillSet discovers skills from a folder at run time.
type FilesystemSkillSet = skills.FilesystemSkillSet

var NewFilesystemSkillSet = skills.NewFilesystemSkillSet

// NewFSSkillSet supports embed.FS and other io/fs implementations.
var NewFSSkillSet = skills.NewFSSkillSet
