package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// filesystemCatalog is a private snapshot of a source folder.
type filesystemCatalog struct {
	skills  []agents.Skill
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

func loadDirectoryCatalog(dirs ...string) (*filesystemCatalog, error) {
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

	return loadCatalog(sources)
}

func loadFSCatalog(filesystems ...fs.FS) (*filesystemCatalog, error) {
	if len(filesystems) == 0 {
		return nil, fmt.Errorf("loading skills: no filesystem given")
	}

	sources := make([]skillSource, 0, len(filesystems))
	for _, fsys := range filesystems {
		sources = append(sources, skillSource{fsys: fsys})
	}

	return loadCatalog(sources)
}

// loadCatalog reads each source in turn. It fails rather than skipping on
// a malformed skill: skills are an asset the agent's behaviour depends on, so
// a bad one is worth surfacing at startup instead of a capability that goes
// quietly missing at runtime.
func loadCatalog(sources []skillSource) (*filesystemCatalog, error) {
	r := &filesystemCatalog{entries: map[string]skillEntry{}}

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
func (r *filesystemCatalog) loadSource(source skillSource) error {
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

// Skills lists the loaded skills, in the order they were found.
func (r *filesystemCatalog) skillsList() []agents.Skill {
	skills := slices.Clone(r.skills)
	for i := range skills {
		skills[i].Resources = slices.Clone(skills[i].Resources)
	}
	return skills
}

// Get returns the named skill's metadata.
func (r *filesystemCatalog) get(name string) (agents.Skill, bool) {
	for _, skill := range r.skills {
		if skill.Name == name {
			return skill, true
		}
	}
	return agents.Skill{}, false
}

// Read returns the named skill's instructions — the SKILL.md body with the
// frontmatter stripped, since the model already has the name and description
// from the prompt.
func (r *filesystemCatalog) read(name string) (string, error) {
	entry, ok := r.entries[name]
	if !ok {
		return "", r.unknownSkill(name)
	}

	data, err := fs.ReadFile(entry.source.fsys, path.Join(entry.dir, agents.SkillFileName))
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
func (r *filesystemCatalog) readFile(name, file string) (string, error) {
	skill, ok := r.get(name)
	if !ok {
		return "", r.unknownSkill(name)
	}

	// Resolving against "/" collapses any ".." before it can escape, so the
	// result is always a path inside the skill folder.
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(file)), "/")
	if rel == "" || rel == "." {
		return "", fmt.Errorf("no file given for skill %q", name)
	}

	if rel != agents.SkillFileName && !slices.Contains(skill.Resources, rel) {
		return "", fmt.Errorf("skill %q has no file %q; it bundles: %s", name, rel, resourceList(skill))
	}

	entry := r.entries[name]
	data, err := fs.ReadFile(entry.source.fsys, path.Join(entry.dir, rel))
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (r *filesystemCatalog) load(source skillSource, dir string) (agents.Skill, error) {
	file := path.Join(dir, agents.SkillFileName)
	location := source.locate(file)

	data, err := fs.ReadFile(source.fsys, file)
	if err != nil {
		return agents.Skill{}, err
	}

	fm, _, err := parseSkillFrontmatter(data)
	if err != nil {
		return agents.Skill{}, fmt.Errorf("%s: %w", location, err)
	}

	name := strings.TrimSpace(fm.Name)
	if name == "" {
		// A skill in its own folder is named after it, which is what lets a
		// skill author get away with frontmatter that only sets a description.
		name = source.folderName(dir)
		if name == "" {
			return agents.Skill{}, fmt.Errorf("%s: skill has no name in its frontmatter and no folder to take one from", location)
		}
	}
	if strings.TrimSpace(fm.Description) == "" {
		// The description is the only thing the model sees before deciding to
		// read the skill, so a skill without one can never be picked.
		return agents.Skill{}, fmt.Errorf("%s: skill %q has no description in its frontmatter", location, name)
	}

	resources, err := listResources(source.fsys, dir)
	if err != nil {
		return agents.Skill{}, err
	}

	return agents.Skill{
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
		if d.IsDir() || (path.Dir(p) == dir && d.Name() == agents.SkillFileName) {
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

func (r *filesystemCatalog) unknownSkill(name string) error {
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
	return e.source.locate(path.Join(e.dir, agents.SkillFileName))
}

func isSkillFolder(fsys fs.FS, dir string) bool {
	info, err := fs.Stat(fsys, path.Join(dir, agents.SkillFileName))
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

func resourceList(skill agents.Skill) string {
	if len(skill.Resources) == 0 {
		return "no files besides " + agents.SkillFileName
	}
	return strings.Join(skill.Resources, ", ")
}
