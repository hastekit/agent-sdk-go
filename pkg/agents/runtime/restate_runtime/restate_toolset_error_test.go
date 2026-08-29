package restate_runtime

import (
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Same round trip as Temporal's, in restate's terms: the kind rides out of the
// step on the error code and is put back outside it.
func TestToolsetAuthErrorSurvivesTheStepBoundary(t *testing.T) {
	original := agents.NewToolsetError(agents.ToolsetErrorAuth, errors.New("initialize: Unauthorized"))

	onTheWire := toolsetListError(original)
	assert.EqualValues(t, ToolsetAuthErrorCode, restate.ErrorCode(onTheWire))

	restored := toolsetListErrorFrom(onTheWire)

	var te *agents.ToolsetError
	require.True(t, errors.As(restored, &te))
	assert.Equal(t, agents.ToolsetErrorAuth, te.Kind)
}

func TestToolsetListErrorLeavesOtherFailuresAlone(t *testing.T) {
	// Not an auth failure: no code of its own, so restate keeps retrying the
	// step as it would any other transient one.
	err := errors.New("dial tcp: connection refused")
	assert.Equal(t, err, toolsetListError(err))
	assert.NotEqualValues(t, ToolsetAuthErrorCode, restate.ErrorCode(toolsetListError(err)))

	var te *agents.ToolsetError
	assert.False(t, errors.As(toolsetListErrorFrom(err), &te))
}
