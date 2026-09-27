package agents

// SkillFileName is the file a skill folder must contain. Everything else in
// the folder is a bundled resource the skill can point the model at.
const SkillFileName = "SKILL.md"

type Skill struct {
	// Global marks a developer-owned skill. Users cannot disable it, and it
	// shadows a user-owned skill with the same name. Set by the SkillClient,
	// never read from uploaded frontmatter.
	Global       bool   `json:"global,omitempty"`
	Name         string `json:"name"`          // Skill name from SKILL.md frontmatter
	Description  string `json:"description"`   // Skill description from SKILL.md frontmatter
	FileLocation string `json:"file_location"` // Path to the SKILL.md file, as a reader would type it

	// Resources are the skill's other files, as paths relative to the skill
	// folder. read_skill serves them by name; nothing outside this list (and
	// SKILL.md itself) is reachable through the tool.
	Resources []string `json:"resources,omitempty"`
}
