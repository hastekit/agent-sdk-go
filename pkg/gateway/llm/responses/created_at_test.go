package responses

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// When a reply was produced reads back the same whether the response stayed
// in process or crossed a durable runtime's boundary as JSON.
func TestCreatedAtSurvivesAJSONRoundTrip(t *testing.T) {
	want := time.Date(2026, 10, 1, 9, 0, 3, 120, time.UTC)

	var r Response
	r.SetCreatedAt(want)
	got, ok, err := r.CreatedAt()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)

	data, err := json.Marshal(r)
	require.NoError(t, err)
	var crossed Response
	require.NoError(t, json.Unmarshal(data, &crossed))
	got, ok, err = crossed.CreatedAt()
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, want.Equal(got))

	_, ok, err = (&Response{}).CreatedAt()
	require.NoError(t, err)
	require.False(t, ok, "a response nobody stamped says so")
}
