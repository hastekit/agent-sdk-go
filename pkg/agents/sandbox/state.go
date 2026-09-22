package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
)

// StateKey holds the authoritative sandbox reference in run.State.
const StateKey = "sandbox_reference"

// State records resource identity and the scope in which it may be reused.
// Credentials, daemon addresses, and creation environment values are excluded.
type State struct {
	Reference Reference  `json:"reference"`
	Session   SessionKey `json:"session"`
	Profile   string     `json:"profile"`
}

func ReadState(state map[string]string) (*State, error) {
	raw := state[StateKey]
	if raw == "" {
		return nil, nil
	}
	var value State
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("invalid sandbox state: %w", err)
	}
	if value.Reference.ID == "" || value.Reference.Provider == "" || value.Session.Namespace == "" || value.Session.SessionID == "" || value.Profile == "" {
		return nil, fmt.Errorf("invalid sandbox reference or scope in run state")
	}
	return &value, nil
}

// Acquire reconnects using run state, or creates when no reference exists.
// Apply the returned updates before the next tool call and persist them between
// runs, including when subsequent work fails. Concurrent runs of one session are
// unsupported. Missing resources and corrupt state are never silently replaced.
func Acquire(ctx context.Context, provider Provider, state map[string]string, req CreateRequest) (Sandbox, map[string]string, error) {
	if provider == nil {
		return nil, nil, fmt.Errorf("sandbox provider is not configured")
	}
	if req.Session.Namespace == "" || req.Session.SessionID == "" || req.Profile == "" || req.TTL < 0 {
		return nil, nil, fmt.Errorf("sandbox requires profile, namespace, session and nonnegative TTL")
	}
	existing, err := ReadState(state)
	if err != nil {
		return nil, nil, err
	}
	var sb Sandbox
	if existing != nil {
		if existing.Session != req.Session || existing.Profile != req.Profile {
			return nil, nil, ErrConflict
		}
		sb, err = provider.Connect(ctx, existing.Reference)
	} else {
		sb, err = provider.Create(ctx, req)
	}
	if err != nil {
		return nil, nil, err
	}
	if sb == nil || sb.Reference().ID == "" || sb.Reference().Provider == "" {
		return nil, nil, fmt.Errorf("invalid sandbox reference")
	}
	if existing != nil && sb.Reference() != existing.Reference {
		return nil, nil, ErrConflict
	}
	data, err := json.Marshal(State{Reference: sb.Reference(), Session: req.Session, Profile: req.Profile})
	if err != nil {
		return nil, nil, err
	}
	return sb, map[string]string{StateKey: string(data)}, nil
}
