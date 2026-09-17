package agents

import (
	"context"
	"fmt"
)

type stubSkillSet struct {
	Name    string
	List    func(context.Context, string, map[string]any) ([]Skill, error)
	Resolve func(context.Context, string, map[string]any, string, string) (string, error)
}

func (s stubSkillSet) GetName() string { return s.Name }
func (s stubSkillSet) ListSkills(ctx context.Context, namespace string, rc map[string]any) ([]Skill, error) {
	if s.List == nil {
		return nil, fmt.Errorf("skill set %q has no list function", s.Name)
	}
	return s.List(ctx, namespace, rc)
}
func (s stubSkillSet) ResolveSkill(ctx context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	if s.Resolve == nil {
		return "", fmt.Errorf("skill set %q has no resolver", s.Name)
	}
	return s.Resolve(ctx, namespace, rc, name, file)
}
