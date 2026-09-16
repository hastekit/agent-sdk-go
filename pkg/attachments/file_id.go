package attachments

import "strings"

// FileID encodes an application-owned reference for a Responses file_id field.
// This is an SDK convention, not a provider file ID or a fetchable URL.
// Attachment middleware resolves it before dispatch; ordinary provider IDs
// are left untouched. The storage backend remains an implementation detail.
func FileID(ref Ref) string {
	return "attachment://" + strings.TrimPrefix(URL(ref), "/attachments/")
}

// IsFileID recognizes the reserved attachment scheme, including malformed
// references which RefFromFileID will reject rather than pass to a provider.
func IsFileID(id string) bool {
	return len(id) >= len("attachment:") && strings.EqualFold(id[:len("attachment:")], "attachment:")
}

// RefFromFileID decodes an SDK attachment ID without accessing storage.
func RefFromFileID(id string) (Ref, error) {
	if !strings.HasPrefix(id, "attachment://") {
		return Ref{}, ErrInvalid
	}
	return RefFromURL("/attachments/" + strings.TrimPrefix(id, "attachment://"))
}
