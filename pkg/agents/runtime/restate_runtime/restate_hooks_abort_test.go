package restate_runtime

import (
	"errors"
	"testing"

	restate "github.com/restatedev/sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A hook's error has to reach Restate as terminal. A non-terminal one makes
// Restate retry the invocation, which replays into the same step and asks a hook
// that has already said no to say it again — the run would hang on the refusal
// rather than ending on it.
func TestAbortError_IsTerminal(t *testing.T) {
	denied := errors.New("tenant mismatch")

	err := abortError(denied)
	require.Error(t, err)

	assert.EqualValues(t, ToolCallAbortedErrorCode, restate.ErrorCode(err))

	// A Restate step runs in this process, so the hook's own error survives the
	// terminal wrapper.
	assert.ErrorIs(t, err, denied)
	assert.Contains(t, err.Error(), "tenant mismatch")
}

func TestAbortError_PassesNilThrough(t *testing.T) {
	assert.NoError(t, abortError(nil))
}

// The code has to be one nothing else produces: Restate answers 500 for any
// error carrying no code of its own, so 500 would make a hook's refusal
// indistinguishable from every other failure to anything reading it back.
func TestAbortedErrorCode_IsDistinctFromTheDefault(t *testing.T) {
	assert.EqualValues(t, 500, restate.ErrorCode(errors.New("no code of its own")))
	assert.NotEqualValues(t, 500, ToolCallAbortedErrorCode)
}
