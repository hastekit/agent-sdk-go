package temporal_runtime

import (
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
)

// A Go error does not survive the activity boundary, so the kind rides over on
// the ApplicationError's type and is put back on the workflow side. Without the
// round trip an unauthorized server would look like any other outage, and the
// agent would tell the user to wait rather than to reconnect.
func TestToolsetAuthErrorSurvivesTheActivityBoundary(t *testing.T) {
	original := agents.NewToolsetError(agents.ToolsetErrorAuth, errors.New("initialize: Unauthorized"))

	onTheWire := toolsetListError(original)

	var appErr *temporal.ApplicationError
	require.True(t, errors.As(onTheWire, &appErr))
	assert.Equal(t, ToolsetAuthErrorType, appErr.Type())
	assert.True(t, appErr.NonRetryable(), "the same credential gets the same answer; retrying only delays the run")

	restored := toolsetListErrorFrom(onTheWire)

	var te *agents.ToolsetError
	require.True(t, errors.As(restored, &te))
	assert.Equal(t, agents.ToolsetErrorAuth, te.Kind)
}

func TestToolsetListErrorLeavesOtherFailuresAlone(t *testing.T) {
	// Not an auth failure: it keeps Temporal's ordinary retry behaviour.
	err := errors.New("dial tcp: connection refused")
	assert.Equal(t, err, toolsetListError(err))

	var appErr *temporal.ApplicationError
	assert.False(t, errors.As(toolsetListError(err), &appErr))

	// And an unrelated application error is not read as one.
	other := temporal.NewNonRetryableApplicationError("nope", "SomethingElse", nil)
	var te *agents.ToolsetError
	assert.False(t, errors.As(toolsetListErrorFrom(other), &te))
}
