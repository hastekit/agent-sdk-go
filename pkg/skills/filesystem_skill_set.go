package skills

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// FilesystemSkillSet discovers SKILL.md folders and their resources on every
// listing. All skills are enabled by default and may be disabled in AgentInput.
// Reads use a fresh catalog, so no mutable registry is shared between runs.
// The filesystem must support concurrent reads; applications own its updates.
type FilesystemSkillSet struct {
	name string
	load func() (*filesystemCatalog, error)
}

// NewFilesystemSkillSet reads skills under directory, including nested folders.
// It validates the initial catalog. New and removed skills are discovered on
// subsequent runs without reconstructing the agent. Skill names are exposed without a source prefix.
func NewFilesystemSkillSet(name, directory string) (*FilesystemSkillSet, error) {
	return newFilesystemSkillSet(name, func() (*filesystemCatalog, error) { return loadDirectoryCatalog(directory) })
}

// NewFSSkillSet is NewFilesystemSkillSet for an embed.FS or another fs.FS.
// Pass fs.Sub(fsys, "skills") when only that subtree should be exposed.
func NewFSSkillSet(name string, fsys fs.FS) (*FilesystemSkillSet, error) {
	if fsys == nil {
		return nil, fmt.Errorf("skill filesystem is nil")
	}
	return newFilesystemSkillSet(name, func() (*filesystemCatalog, error) { return loadFSCatalog(fsys) })
}

func newFilesystemSkillSet(name string, load func() (*filesystemCatalog, error)) (*FilesystemSkillSet, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid skill set name %q", name)
	}
	if _, err := load(); err != nil {
		return nil, err
	}
	return &FilesystemSkillSet{name: name, load: load}, nil
}

func (s *FilesystemSkillSet) GetName() string { return s.name }
func (s *FilesystemSkillSet) ListSkills(ctx context.Context, _ string, _ map[string]any) ([]agents.Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	catalog, err := s.load()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	skills := catalog.skillsList()
	for i := range skills {
		skills[i].Policy = agents.SkillEnabled
	}
	return skills, nil
}
func (s *FilesystemSkillSet) ResolveSkill(ctx context.Context, _ string, _ map[string]any, name, file string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	catalog, err := s.load()
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if file == "" {
		return catalog.read(name)
	}
	return catalog.readFile(name, file)
}
