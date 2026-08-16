package agents

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillFileName is the file a skill folder must contain. Everything else in
// the folder is a bundled resource the skill can point the model at.
const SkillFileName = "SKILL.md"

type Skill struct {
	Name         string `json:"name"`          // Skill name from SKILL.md frontmatter
	Description  string `json:"description"`   // Skill description from SKILL.md frontmatter
	FileLocation string `json:"file_location"` // Path to the SKILL.md file, as a reader would type it

	// Resources are the skill's other files, as paths relative to the skill
	// folder. read_skill serves them by name; nothing outside this list (and
	// SKILL.md itself) is reachable through the tool.
	Resources []string `json:"resources,omitempty"`
}

// SkillProvider is where an agent's skills come from. Give one to
// AgentOptions.Skills and the agent takes it from there: it lists the skills
// in the prompt and adds the source's own reader tool to its tools, so there
// is no way to end up advertising skills the model has no way to read.
//
// *SkillRegistry is the usual implementation. Implement it yourself for skills
// that live somewhere the registry can't reach, or that are read by a tool of
// your own.
type SkillProvider interface {
	// Skills lists what the model should be told it can reach for.
	Skills() []Skill

	// SkillTool is the tool that reads them. Return nil when the model
	// already has a way to read them — skills sitting in a sandbox the agent
	// browses with its bash tool need no tool of their own.
	SkillTool() Tool
}

// SkillList is a SkillProvider for skills the agent can already reach by other
// means — ones staged into a sandbox it browses with its bash tool, say. It
// lists them for the model and adds no tool.
type SkillList []Skill

func (s SkillList) Skills() []Skill { return append([]Skill(nil), s...) }
func (s SkillList) SkillTool() Tool { return nil }

// SkillRegistry holds the skills found in one or more places. A skill is a
// folder holding a SKILL.md with YAML frontmatter (name, description) followed
// by the instructions themselves.
//
// Point it at a directory on disk, which is what you want while the skills are
// still being written — editing a SKILL.md and restarting is the whole loop:
//
//	registry, err := agents.NewSkillRegistryFromDir("./skills")
//
// Or embed the folder, so the skills ship inside the binary and need no disk,
// no volume mount, and no deploy step of their own:
//
//	//go:embed skills
//	var skillsFS embed.FS
//
//	registry, err := agents.NewSkillRegistry(skillsFS)
//
// Either way the reading happens once, here. The registry is read-only
// afterwards and safe for concurrent use; to pick up edits on disk, build a
// new one.
type SkillRegistry struct {
	skills  []Skill
	entries map[string]skillEntry // skill name -> where it came from
}

// skillSource is one filesystem the registry reads. root is only a label: it
// prefixes FileLocation so a skill loaded from disk reports the path its
// author would type, rather than one relative to an fs.FS they never saw.
type skillSource struct {
	fsys fs.FS
	root string
}

type skillEntry struct {
	source skillSource
	dir    string // the skill's folder within source.fsys
}

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// NewSkillRegistryFromDir reads every skill in the given directories — the
// plain case, where the skills sit in a folder next to the binary or on a
// mounted volume:
//
//	registry, err := agents.NewSkillRegistryFromDir("./skills")
//
// Pass several directories to draw from more than one library (a shared set
// plus this agent's own, say); a name defined twice is an error rather than a
// silent win for whichever was read last.
func NewSkillRegistryFromDir(dirs ...string) (*SkillRegistry, error) {
	if len(dirs) == 0 {
		return nil, fmt.Errorf("loading skills: no directory given")
	}

	sources := make([]skillSource, 0, len(dirs))
	for _, dir := range dirs {
		// os.DirFS defers every error to first use, so a typo in the path
		// would otherwise surface as an agent with no skills rather than as a
		// failure to start.
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("loading skills from %s: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("loading skills from %s: not a directory", dir)
		}
		sources = append(sources, skillSource{fsys: os.DirFS(dir), root: dir})
	}

	return newSkillRegistry(sources)
}

// NewSkillRegistry reads every skill in the given filesystems — an embed.FS,
// or anything else that implements fs.FS. For a directory on disk prefer
// NewSkillRegistryFromDir, which checks the path and reports locations the way
// its author wrote them.
func NewSkillRegistry(filesystems ...fs.FS) (*SkillRegistry, error) {
	if len(filesystems) == 0 {
		return nil, fmt.Errorf("loading skills: no filesystem given")
	}

	sources := make([]skillSource, 0, len(filesystems))
	for _, fsys := range filesystems {
		sources = append(sources, skillSource{fsys: fsys})
	}

	return newSkillRegistry(sources)
}

// newSkillRegistry reads each source in turn. It fails rather than skipping on
// a malformed skill: skills are an asset the agent's behaviour depends on, so
// a bad one is worth surfacing at startup instead of a capability that goes
// quietly missing at runtime.
func newSkillRegistry(sources []skillSource) (*SkillRegistry, error) {
	r := &SkillRegistry{entries: map[string]skillEntry{}}

	for _, source := range sources {
		if err := r.loadSource(source); err != nil {
			return nil, fmt.Errorf("loading skills: %w", err)
		}
	}

	return r, nil
}

// loadSource registers every skill folder in one source. A folder holding a
// SKILL.md is a skill and the whole subtree below it belongs to that skill —
// so a SKILL.md a skill bundles as an example or a template stays a bundled
// file, and never becomes a second, half-formed skill of its own.
//
// The search does descend to find them, so pointing at a parent folder works:
// //go:embed skills gives a filesystem whose root holds skills/, not the
// skills themselves, and there is no fs.Sub to get right.
func (r *SkillRegistry) loadSource(source skillSource) error {
	return fs.WalkDir(source.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || !isSkillFolder(source.fsys, p) {
			return nil
		}

		skill, err := r.load(source, p)
		if err != nil {
			return err
		}
		if existing, dup := r.entries[skill.Name]; dup {
			return fmt.Errorf("skill %q is defined twice: %s and %s",
				skill.Name, existing.location(), skill.FileLocation)
		}

		r.entries[skill.Name] = skillEntry{source: source, dir: p}
		r.skills = append(r.skills, skill)

		return fs.SkipDir
	})
}

// Skills lists the loaded skills, in the order they were found. It is half of
// SkillProvider: the agent puts these in the prompt.
func (r *SkillRegistry) Skills() []Skill {
	return append([]Skill(nil), r.skills...)
}

// SkillTool is the other half of SkillProvider: the read_skill tool bound to
// this registry, which the agent adds to its own tools.
func (r *SkillRegistry) SkillTool() Tool {
	return NewReadSkillTool(r)
}

// Get returns the named skill's metadata.
func (r *SkillRegistry) Get(name string) (Skill, bool) {
	for _, skill := range r.skills {
		if skill.Name == name {
			return skill, true
		}
	}
	return Skill{}, false
}

// Read returns the named skill's instructions — the SKILL.md body with the
// frontmatter stripped, since the model already has the name and description
// from the prompt.
func (r *SkillRegistry) Read(name string) (string, error) {
	entry, ok := r.entries[name]
	if !ok {
		return "", r.unknownSkill(name)
	}

	data, err := fs.ReadFile(entry.source.fsys, path.Join(entry.dir, SkillFileName))
	if err != nil {
		return "", err
	}

	_, body, err := parseSkillFrontmatter(data)
	if err != nil {
		return "", err
	}

	return body, nil
}

// ReadFile returns one of the skill's bundled resources, named relative to the
// skill folder. Only files the skill actually bundles are reachable: a
// traversing path resolves inside the folder first and then has to match a
// known resource, so it cannot reach another skill or the rest of the
// filesystem the skills were read from.
func (r *SkillRegistry) ReadFile(name, file string) (string, error) {
	skill, ok := r.Get(name)
	if !ok {
		return "", r.unknownSkill(name)
	}

	// Resolving against "/" collapses any ".." before it can escape, so the
	// result is always a path inside the skill folder.
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(file)), "/")
	if rel == "" || rel == "." {
		return "", fmt.Errorf("no file given for skill %q", name)
	}

	if rel != SkillFileName && !slices.Contains(skill.Resources, rel) {
		return "", fmt.Errorf("skill %q has no file %q; it bundles: %s", name, rel, resourceList(skill))
	}

	entry := r.entries[name]
	data, err := fs.ReadFile(entry.source.fsys, path.Join(entry.dir, rel))
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (r *SkillRegistry) load(source skillSource, dir string) (Skill, error) {
	file := path.Join(dir, SkillFileName)
	location := source.locate(file)

	data, err := fs.ReadFile(source.fsys, file)
	if err != nil {
		return Skill{}, err
	}

	fm, _, err := parseSkillFrontmatter(data)
	if err != nil {
		return Skill{}, fmt.Errorf("%s: %w", location, err)
	}

	name := strings.TrimSpace(fm.Name)
	if name == "" {
		// A skill in its own folder is named after it, which is what lets a
		// skill author get away with frontmatter that only sets a description.
		name = source.folderName(dir)
		if name == "" {
			return Skill{}, fmt.Errorf("%s: skill has no name in its frontmatter and no folder to take one from", location)
		}
	}
	if strings.TrimSpace(fm.Description) == "" {
		// The description is the only thing the model sees before deciding to
		// read the skill, so a skill without one can never be picked.
		return Skill{}, fmt.Errorf("%s: skill %q has no description in its frontmatter", location, name)
	}

	resources, err := listResources(source.fsys, dir)
	if err != nil {
		return Skill{}, err
	}

	return Skill{
		Name:         name,
		Description:  strings.TrimSpace(fm.Description),
		FileLocation: location,
		Resources:    resources,
	}, nil
}

// listResources collects the skill's files other than SKILL.md, relative to
// the skill folder.
func listResources(fsys fs.FS, dir string) ([]string, error) {
	var resources []string

	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (path.Dir(p) == dir && d.Name() == SkillFileName) {
			return nil
		}

		rel, err := relTo(dir, p)
		if err != nil {
			return err
		}
		resources = append(resources, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return resources, nil
}

func (r *SkillRegistry) unknownSkill(name string) error {
	names := make([]string, 0, len(r.skills))
	for _, skill := range r.skills {
		names = append(names, skill.Name)
	}
	if len(names) == 0 {
		return fmt.Errorf("unknown skill %q: no skills are loaded", name)
	}
	return fmt.Errorf("unknown skill %q; available skills: %s", name, strings.Join(names, ", "))
}

// locate turns a path inside the source into one its author would recognise.
func (s skillSource) locate(p string) string {
	if s.root == "" {
		return p
	}
	return filepath.Join(s.root, filepath.FromSlash(p))
}

// folderName is the name a skill takes when its frontmatter gives none. A
// skill at the root of its source is named after the directory that was
// pointed at, so pointing straight at one skill folder works.
func (s skillSource) folderName(dir string) string {
	if dir != "." {
		return path.Base(dir)
	}
	if s.root == "" {
		return ""
	}
	return filepath.Base(s.root)
}

func (e skillEntry) location() string {
	return e.source.locate(path.Join(e.dir, SkillFileName))
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

func isSkillFolder(fsys fs.FS, dir string) bool {
	info, err := fs.Stat(fsys, path.Join(dir, SkillFileName))
	return err == nil && !info.IsDir()
}

func relTo(dir, p string) (string, error) {
	if dir == "." {
		return p, nil
	}
	rel := strings.TrimPrefix(p, dir+"/")
	if rel == p {
		return "", fmt.Errorf("%s is not inside %s", p, dir)
	}
	return rel, nil
}

func resourceList(skill Skill) string {
	if len(skill.Resources) == 0 {
		return "no files besides " + SkillFileName
	}
	return strings.Join(skill.Resources, ", ")
}
