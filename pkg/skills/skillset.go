package skills

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

type NamespaceResolver func(context.Context, string, map[string]any) (string, error)
type SetOption func(*StoredSkillSet)

// StoredSkillSet adapts any Store into the agent's single read_skill tool.
// Uploaded skills are opt-in by default; host configuration owns all policies.
type StoredSkillSet struct {
	name     string
	store    Store
	resolve  NamespaceResolver
	policy   agents.SkillPolicy
	policies map[string]agents.SkillPolicy
}

func WithDefaultPolicy(policy agents.SkillPolicy) SetOption {
	return func(s *StoredSkillSet) { s.policy = policy }
}
func WithPolicies(policies map[string]agents.SkillPolicy) SetOption {
	return func(s *StoredSkillSet) { s.policies = maps.Clone(policies) }
}
func WithNamespaceResolver(resolve NamespaceResolver) SetOption {
	return func(s *StoredSkillSet) { s.resolve = resolve }
}

// NewSkillSet uses the explicit namespace, falling back to "default" when empty.
// WithNamespaceResolver can override it for shared built-ins. Configuration is immutable.
func NewSkillSet(name string, store Store, opts ...SetOption) (*StoredSkillSet, error) {
	if !validName(name) || store == nil {
		return nil, fmt.Errorf("%w: source name and store required", ErrInvalid)
	}
	s := &StoredSkillSet{name: name, store: store, policy: agents.SkillOptIn, resolve: func(_ context.Context, namespace string, rc map[string]any) (string, error) {
		if strings.TrimSpace(namespace) != "" {
			return namespace, nil
		}
		return "default", nil
	}}
	for _, opt := range opts {
		if opt == nil {
			return nil, ErrInvalid
		}
		opt(s)
	}
	if s.resolve == nil {
		return nil, fmt.Errorf("%w: namespace resolver required", ErrInvalid)
	}
	valid := func(p agents.SkillPolicy) bool {
		return p == "" || p == agents.SkillOptIn || p == agents.SkillEnabled || p == agents.SkillRequired || p == agents.SkillBlocked
	}
	if !valid(s.policy) {
		return nil, fmt.Errorf("%w: policy", ErrInvalid)
	}
	for name, p := range s.policies {
		if !validName(name) || !valid(p) {
			return nil, fmt.Errorf("%w: policy for %q", ErrInvalid, name)
		}
	}
	return s, nil
}
func (s *StoredSkillSet) GetName() string { return s.name }
func (s *StoredSkillSet) ListSkills(ctx context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	ns, err := s.resolve(ctx, namespace, rc)
	if err != nil {
		return nil, err
	}
	var result []agents.Skill
	cursor := ""
	seen := map[string]bool{}
	for {
		page, err := s.store.List(ctx, ns, ListOptions{Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, m := range page.Skills {
			p := s.policy
			if override, ok := s.policies[m.Name]; ok {
				p = override
			}
			result = append(result, agents.Skill{Name: m.Name, Description: m.Description, Resources: m.Resources, Policy: p})
		}
		if page.NextCursor == "" {
			return result, nil
		}
		if seen[page.NextCursor] {
			return nil, fmt.Errorf("skill store repeated pagination cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}
func (s *StoredSkillSet) ResolveSkill(ctx context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	ns, err := s.resolve(ctx, namespace, rc)
	if err != nil {
		return "", err
	}
	b, err := s.store.Get(ctx, ns, name)
	if err != nil {
		return "", err
	}
	instructions := file == ""
	if instructions {
		file = "SKILL.md"
	}
	data, ok := b.Files[file]
	if !ok {
		return "", ErrNotFound
	}
	if file == "SKILL.md" && instructions {
		_, body, err := parseSkillFrontmatter(data)
		return body, err
	}
	return string(data), nil
}

var _ agents.SkillSet = (*StoredSkillSet)(nil)
