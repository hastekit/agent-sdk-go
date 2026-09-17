package skills

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"testing/fstest"
)

func TestSkillSourcesShareInstructionParsing(t *testing.T) {
	for _, document := range []string{
		"---\nname: review\ndescription: Review changes\n---\n\nInstructions\n",
		"\ufeff---\r\nname: review\r\ndescription: Review changes\r\n---\r\n\r\nInstructions\r\n",
	} {
		t.Run(document[:3], func(t *testing.T) {
			ctx := context.Background()
			folder, err := NewFSSkillSet("folder", fstest.MapFS{"review/SKILL.md": {Data: []byte(document)}})
			require.NoError(t, err)
			store, err := NewFileStore(t.TempDir())
			require.NoError(t, err)
			_, err = store.Put(ctx, "default", Bundle{Files: map[string][]byte{"SKILL.md": []byte(document)}})
			require.NoError(t, err)
			stored, err := NewSkillSet("stored", store)
			require.NoError(t, err)
			for _, file := range []string{"", "SKILL.md"} {
				a, err := folder.ResolveSkill(ctx, "default", nil, "review", file)
				require.NoError(t, err)
				b, err := stored.ResolveSkill(ctx, "default", nil, "review", file)
				require.NoError(t, err)
				require.Equal(t, a, b)
				if file == "" {
					require.Equal(t, "Instructions\n", a)
				} else {
					require.Equal(t, document, a)
				}
			}
		})
	}
}
