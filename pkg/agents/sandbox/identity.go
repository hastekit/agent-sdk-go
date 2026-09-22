package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/google/uuid"
)

// ResourceID gives providers with unique resource names a stable session identity.
// It does not provide synchronization or make overlapping session runs safe.
func ResourceID(session SessionKey) string {
	data, _ := json.Marshal(session)
	return uuid.NewSHA1(uuid.NameSpaceURL, append([]byte("hastekit:sandbox:"), data...)).String()
}

// RequestFingerprint lets a provider reject a name conflict for a different
// creation request without storing environment values in resource labels.
func RequestFingerprint(req CreateRequest) string {
	data, _ := json.Marshal(req)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:20])
}
