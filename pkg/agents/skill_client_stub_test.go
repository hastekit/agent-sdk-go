package agents

import (
	"context"
	"fmt"
)

type stubSkillClient struct {
	List func(context.Context, string, map[string]any) ([]Skill, error)
	Read func(context.Context, string, map[string]any, string, string) (string, error)
}

func (s stubSkillClient) ListSkills(ctx context.Context, namespace string, rc map[string]any) ([]Skill, error) {
	if s.List == nil {
		return nil, fmt.Errorf("skill client has no list function")
	}
	return s.List(ctx, namespace, rc)
}

func (s stubSkillClient) ReadSkill(ctx context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	if s.Read == nil {
		return "", fmt.Errorf("skill client has no reader")
	}
	return s.Read(ctx, namespace, rc, name, file)
}
