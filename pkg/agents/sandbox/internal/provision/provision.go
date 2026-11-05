// Package provision contains configuration helpers for providers that create
// their own containers.
package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

const ManagedKey = "hastekit.managed"
const ManagedValue = "sandbox-sdk"
const FingerprintKey = "hastekit.configuration"

// StartupScriptKey holds a profile's startup script on the resource it created. A
// reference carries no profile, and this makes it a creation setting like the rest,
// so a changed script does not reach existing compute.
const StartupScriptKey = "hastekit.startup-script"

func Validate(req sandbox.CreateRequest) error {
	if req.Profile == "" || req.Session.Namespace == "" || req.Session.SessionID == "" {
		return fmt.Errorf("sandbox profile, namespace, and session are required")
	}
	if req.TTL != 0 {
		return fmt.Errorf("%w: provider TTL is unsupported; use the provider's IdleTimeout and MaxLifetime", sandbox.ErrUnsupported)
	}
	_, err := sandbox.ValidateExec(sandbox.ExecRequest{Argv: []string{"sandbox"}, Env: req.Env})
	return err
}

func Fingerprint(req sandbox.CreateRequest, spec any) (string, error) {
	data, err := json.Marshal(struct {
		Request sandbox.CreateRequest
		Spec    any
	}{req, spec})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:20]), nil
}

// Clone isolates native configuration maps and slices from per-call changes.
func Clone[T any](value T) (T, error) {
	var result T
	data, err := json.Marshal(value)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(data, &result)
	return result, err
}
