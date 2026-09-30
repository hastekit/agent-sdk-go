package agui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPSelectionInput(t *testing.T) {
	// AG-UI forwards server enablement and original tool-name filters without deriving identity from them.
	input := RunAgentInput{ForwardedProps: map[string]any{"mcp": map[string]any{
		"enable": []string{"mail"}, "disable": []string{"calendar"},
		"tools": map[string]any{"mail": map[string]any{"include": []string{"read"}, "exclude": []string{"send"}}},
	}}}
	selection, err := input.MCPSelection()
	require.NoError(t, err)
	require.Equal(t, []string{"mail"}, selection.Enable)
	require.Equal(t, []string{"calendar"}, selection.Disable)
	require.Equal(t, []string{"read"}, selection.Tools["mail"].Include)
	require.Equal(t, []string{"send"}, selection.Tools["mail"].Exclude)

	// Invalid selection is rejected before the handler claims or starts a run.
	input.ForwardedProps = map[string]any{"mcp": map[string]any{"tools": []string{"invalid"}}}
	_, err = input.MCPSelection()
	require.ErrorContains(t, err, "invalid forwardedProps.mcp")
}
