package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
)

func skillFS() fstest.MapFS {
	return fstest.MapFS{
		"skills/pdf/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: pdf\ndescription: Fill and read PDF forms.\n---\n\nUse pdftk for forms.\n")},
		"skills/pdf/references/forms.md": &fstest.MapFile{Data: []byte("field syntax\n")},
		// No name in the frontmatter: the folder supplies it.
		"skills/invoice/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\ndescription: Produce an invoice.\n---\n\nTotals go at the bottom.\n")},
	}
}

func newRegistry(t *testing.T, fsys fstest.MapFS) *agents.SkillRegistry {
	t.Helper()

	registry, err := agents.NewSkillRegistry(fsys)
	if err != nil {
		t.Fatalf("NewSkillRegistry: %v", err)
	}
	return registry
}

func TestSkillRegistryLoadsEmbeddedFolders(t *testing.T) {
	registry := newRegistry(t, skillFS())

	skills := registry.Skills()
	if len(skills) != 2 {
		t.Fatalf("got %d skills, want 2: %+v", len(skills), skills)
	}

	pdf, ok := registry.Get("pdf")
	if !ok {
		t.Fatal("skill pdf not found")
	}
	if pdf.Description != "Fill and read PDF forms." {
		t.Errorf("description = %q", pdf.Description)
	}
	if pdf.FileLocation != "skills/pdf/SKILL.md" {
		t.Errorf("file location = %q", pdf.FileLocation)
	}
	if len(pdf.Resources) != 1 || pdf.Resources[0] != "references/forms.md" {
		t.Errorf("resources = %v", pdf.Resources)
	}

	// A skill that names nothing takes its name from its folder.
	if _, ok := registry.Get("invoice"); !ok {
		t.Error("skill invoice not found")
	}
}

func TestSkillRegistryReadStripsFrontmatter(t *testing.T) {
	registry := newRegistry(t, skillFS())

	body, err := registry.Read("pdf")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strings.Contains(body, "description:") {
		t.Errorf("frontmatter leaked into the body: %q", body)
	}
	if !strings.Contains(body, "Use pdftk for forms.") {
		t.Errorf("body = %q", body)
	}
}

func TestSkillRegistryReadFileServesBundledResources(t *testing.T) {
	registry := newRegistry(t, skillFS())

	content, err := registry.ReadFile("pdf", "references/forms.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "field syntax\n" {
		t.Errorf("content = %q", content)
	}
}

func TestSkillRegistryReadFileRejectsEscapes(t *testing.T) {
	registry := newRegistry(t, skillFS())

	// Both of these resolve outside the pdf folder; neither may be served.
	for _, file := range []string{"../invoice/SKILL.md", "/etc/passwd"} {
		if _, err := registry.ReadFile("pdf", file); err == nil {
			t.Errorf("ReadFile(%q) succeeded, want refusal", file)
		}
	}
}

func TestSkillRegistryUnknownSkillListsWhatExists(t *testing.T) {
	registry := newRegistry(t, skillFS())

	_, err := registry.Read("spreadsheet")
	if err == nil {
		t.Fatal("Read of an unknown skill succeeded")
	}
	if !strings.Contains(err.Error(), "pdf") {
		t.Errorf("error does not name the available skills: %v", err)
	}
}

func TestSkillRegistryRejectsMalformedSkills(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"no description": {
			"skills/a/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: a\n---\n\nbody\n")},
		},
		"unterminated frontmatter": {
			"skills/a/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: a\ndescription: d\n\nbody\n")},
		},
		"duplicate names": {
			"one/dup/SKILL.md": &fstest.MapFile{Data: []byte("---\ndescription: d\n---\nbody\n")},
			"two/dup/SKILL.md": &fstest.MapFile{Data: []byte("---\ndescription: d\n---\nbody\n")},
		},
	}

	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := agents.NewSkillRegistry(fsys); err == nil {
				t.Error("NewSkillRegistry succeeded, want an error")
			}
		})
	}
}

// A SKILL.md a skill bundles — an example, a template — belongs to that skill.
// Registering it as a second skill of its own is how a template with no real
// description would take the whole registry down at startup.
func TestSkillRegistryTreatsNestedSkillFilesAsResources(t *testing.T) {
	registry := newRegistry(t, fstest.MapFS{
		"outer/SKILL.md":           &fstest.MapFile{Data: []byte("---\nname: outer\ndescription: d\n---\nbody\n")},
		"outer/templates/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: <name>\n---\ntemplate\n")},
	})

	if len(registry.Skills()) != 1 {
		t.Fatalf("got %d skills, want 1: %+v", len(registry.Skills()), registry.Skills())
	}

	outer, _ := registry.Get("outer")
	if len(outer.Resources) != 1 || outer.Resources[0] != "templates/SKILL.md" {
		t.Errorf("resources = %v", outer.Resources)
	}

	content, err := registry.ReadFile("outer", "templates/SKILL.md")
	if err != nil || content != "---\nname: <name>\n---\ntemplate\n" {
		t.Errorf("ReadFile = %q, %v", content, err)
	}
}

func TestSkillRegistryFromDirReadsSkillsOffDisk(t *testing.T) {
	dir := writeSkillDir(t, map[string]string{
		"changelog/SKILL.md":            "---\nname: changelog\ndescription: Write a release changelog entry.\n---\n\nGroup by Added and Fixed.\n",
		"changelog/references/style.md": "no trailing full stop\n",
	})

	registry, err := agents.NewSkillRegistryFromDir(dir)
	if err != nil {
		t.Fatalf("NewSkillRegistryFromDir: %v", err)
	}

	changelog, ok := registry.Get("changelog")
	if !ok {
		t.Fatal("skill changelog not found")
	}
	// The location is the path its author would type, not one relative to an
	// fs.FS they never saw.
	if want := filepath.Join(dir, "changelog", "SKILL.md"); changelog.FileLocation != want {
		t.Errorf("file location = %q, want %q", changelog.FileLocation, want)
	}

	body, err := registry.Read("changelog")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(body, "Group by Added and Fixed.") {
		t.Errorf("body = %q", body)
	}

	style, err := registry.ReadFile("changelog", "references/style.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if style != "no trailing full stop\n" {
		t.Errorf("style = %q", style)
	}
}

func TestSkillRegistryFromDirMergesLibraries(t *testing.T) {
	shared := writeSkillDir(t, map[string]string{
		"changelog/SKILL.md": "---\ndescription: Write a release changelog entry.\n---\nbody\n",
	})
	own := writeSkillDir(t, map[string]string{
		"triage/SKILL.md": "---\ndescription: Triage an incoming bug report.\n---\nbody\n",
	})

	registry, err := agents.NewSkillRegistryFromDir(shared, own)
	if err != nil {
		t.Fatalf("NewSkillRegistryFromDir: %v", err)
	}
	if len(registry.Skills()) != 2 {
		t.Fatalf("got %d skills, want 2: %+v", len(registry.Skills()), registry.Skills())
	}

	// The same name in two libraries is an error, not a silent win for
	// whichever was read last.
	clash := writeSkillDir(t, map[string]string{
		"changelog/SKILL.md": "---\ndescription: A different changelog skill.\n---\nbody\n",
	})
	if _, err := agents.NewSkillRegistryFromDir(shared, clash); err == nil {
		t.Error("NewSkillRegistryFromDir accepted a duplicate skill name")
	}
}

// Pointing straight at one skill's folder works: the skill takes its name from
// the directory that was pointed at.
func TestSkillRegistryFromDirAcceptsASingleSkillFolder(t *testing.T) {
	dir := writeSkillDir(t, map[string]string{
		"changelog/SKILL.md": "---\ndescription: Write a release changelog entry.\n---\nbody\n",
	})

	registry, err := agents.NewSkillRegistryFromDir(filepath.Join(dir, "changelog"))
	if err != nil {
		t.Fatalf("NewSkillRegistryFromDir: %v", err)
	}
	if _, ok := registry.Get("changelog"); !ok {
		t.Errorf("skill changelog not found: %+v", registry.Skills())
	}
}

func TestSkillRegistryFromDirRejectsABadPath(t *testing.T) {
	if _, err := agents.NewSkillRegistryFromDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("NewSkillRegistryFromDir accepted a directory that does not exist")
	}
}

func writeSkillDir(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return root
}
