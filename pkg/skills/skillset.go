package skills

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

type SetOption func(*StoredSkillSet)

// StoredSkillSet adapts any Store into the agent's single read_skill tool.
// Uploaded skills are opt-in by default; host configuration owns all policies.
type StoredSkillSet struct {
	globalNamespace string
	name            string
	store           Store
	policy          agents.SkillPolicy
	policies        map[string]agents.SkillPolicy
}

func WithDefaultPolicy(policy agents.SkillPolicy) SetOption {
	return func(s *StoredSkillSet) { s.policy = policy }
}
func WithPolicies(policies map[string]agents.SkillPolicy) SetOption {
	return func(s *StoredSkillSet) { s.policies = maps.Clone(policies) }
}

// WithGlobalNamespace additionally lists skills from namespace. Caller-owned
// skills take precedence on name collisions. Empty disables shared skills.
// This read-only setting does not change the namespace used by upload handlers.
func WithGlobalNamespace(namespace string) SetOption {
	return func(s *StoredSkillSet) { s.globalNamespace = namespace }
}

// NewSkillSet uses the caller's namespace, falling back to "default" when empty.
// WithGlobalNamespace optionally adds shared skills. Configuration is immutable.
func NewSkillSet(name string, store Store, opts ...SetOption) (*StoredSkillSet, error) {
	if !validName(name) || store == nil {
		return nil, fmt.Errorf("%w: source name and store required", ErrInvalid)
	}
	s := &StoredSkillSet{name: name, store: store, policy: agents.SkillOptIn}
	for _, opt := range opts {
		if opt == nil {
			return nil, ErrInvalid
		}
		opt(s)
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
func (s *StoredSkillSet) ListSkills(ctx context.Context, namespace string, _ map[string]any) ([]agents.Skill, error) {
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}
	namespaces := []string{namespace}
	if s.globalNamespace != "" && s.globalNamespace != namespace {
		namespaces = append(namespaces, s.globalNamespace)
	}
	var result []agents.Skill
	names := map[string]bool{}
	for _, ns := range namespaces {
		cursor := ""
		seen := map[string]bool{}
		for {
			page, err := s.store.List(ctx, ns, ListOptions{Limit: 200, Cursor: cursor})
			if err != nil {
				return nil, err
			}
			for _, m := range page.Skills {
				if names[m.Name] {
					continue
				}
				names[m.Name] = true
				p := s.policy
				if override, ok := s.policies[m.Name]; ok {
					p = override
				}
				result = append(result, agents.Skill{Name: m.Name, Description: m.Description, Resources: m.Resources, Policy: p})
			}
			if page.NextCursor == "" {
				break
			}
			if seen[page.NextCursor] {
				return nil, fmt.Errorf("skill store repeated pagination cursor")
			}
			seen[page.NextCursor] = true
			cursor = page.NextCursor
		}
	}
	return result, nil
}

func (s *StoredSkillSet) ResolveSkill(ctx context.Context, namespace string, _ map[string]any, name, file string) (string, error) {
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}
	b, err := s.store.Get(ctx, namespace, name)
	// Fall back for a missing bundle only, never for a missing resource or an
	// authorization/storage error in the caller's namespace.
	if errors.Is(err, ErrNotFound) && s.globalNamespace != "" && s.globalNamespace != namespace {
		b, err = s.store.Get(ctx, s.globalNamespace, name)
	}
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
