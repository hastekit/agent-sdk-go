package skills

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	defaultEnabled  bool
	required        []string
}

// WithDefaultEnabled sets availability before user choices. The default is false.
func WithDefaultEnabled(enabled bool) SetOption {
	return func(s *StoredSkillSet) { s.defaultEnabled = enabled }
}

// WithRequiredSkills names global skills users cannot disable. User-owned skills
// are always optional, even if their names appear here.
func WithRequiredSkills(names ...string) SetOption {
	return func(s *StoredSkillSet) { s.required = slices.Clone(names) }
}

// WithGlobalNamespace additionally lists skills from namespace. Global
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
	s := &StoredSkillSet{name: name, store: store}
	for _, opt := range opts {
		if opt == nil {
			return nil, ErrInvalid
		}
		opt(s)
	}
	for _, name := range s.required {
		if !validName(name) {
			return nil, fmt.Errorf("%w: required skill %q", ErrInvalid, name)
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
		namespaces = []string{s.globalNamespace, namespace}
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
				global := ns == s.globalNamespace
				result = append(result, agents.Skill{Name: m.Name, Description: m.Description, Resources: m.Resources,
					Global: global, Required: global && slices.Contains(s.required, m.Name), DefaultEnabled: s.defaultEnabled})
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
	first := namespace
	if s.globalNamespace != "" {
		first = s.globalNamespace
	}
	b, err := s.store.Get(ctx, first, name)
	// A global bundle wins in its entirety. Fall back only when it is absent,
	// never for missing resources or storage/authorization errors.
	if errors.Is(err, ErrNotFound) && first != namespace {
		b, err = s.store.Get(ctx, namespace, name)
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
