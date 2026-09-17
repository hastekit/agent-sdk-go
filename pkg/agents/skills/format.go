package skills

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parseSkillFrontmatter splits a SKILL.md into its YAML frontmatter and the
// instructions below it. A file with no frontmatter is all body.
func parseSkillFrontmatter(data []byte) (skillFrontmatter, string, error) {
	var fm skillFrontmatter

	text := strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fm, text, nil
	}

	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &fm); err != nil {
			return fm, "", fmt.Errorf("invalid frontmatter: %w", err)
		}
		return fm, strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\n"), nil
	}

	return fm, "", fmt.Errorf("frontmatter is missing its closing ---")
}
