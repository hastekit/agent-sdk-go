package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/oauth2"
)

var ErrCredentialNotFound = errors.New("MCP credential not found")

// CredentialStore persists tokens for exactly one namespace and credential key.
// Namespace is the authenticated user's subject, not a conversation or session ID.
// The key is an opaque OAuthCredentialKey; treat it as a string, never parse it.
type CredentialStore interface {
	Load(context.Context, string, string) (*oauth2.Token, error)
	Save(context.Context, string, string, *oauth2.Token) error
	Delete(context.Context, string, string) error
}

// FileCredentialStore stores private plaintext token files with atomic replacement.
// Use a private local directory; this is not an encrypted vault or a distributed refresh lock.
type FileCredentialStore struct {
	root *os.Root
}

// NewFileCredentialStore opens a directory restricted to its owner.
func NewFileCredentialStore(directory string) (*FileCredentialStore, error) {
	// Require a dedicated directory rather than accidentally chmod-ing the working directory.
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("MCP credential directory is required")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, err
	}

	// Root-relative operations keep all records confined to the configured directory.
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	return &FileCredentialStore{root: root}, nil
}

// Close releases the directory handle after the provider and its MCP clients stop using it.
func (s *FileCredentialStore) Close() error { return s.root.Close() }

// credentialKey encodes a tuple without delimiter collisions or user-controlled path components.
func credentialKey(namespace, server string) (string, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(server) == "" || !utf8.ValidString(namespace) || !utf8.ValidString(server) {
		return "", errors.New("MCP credentials require a namespace and server name")
	}
	data, _ := json.Marshal([2]string{namespace, server})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// Load returns the latest token without retaining credential values in a shared cache.
func (s *FileCredentialStore) Load(ctx context.Context, namespace, server string) (*oauth2.Token, error) {
	// Check cancellation and scope before touching any file.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := credentialKey(namespace, server)
	if err != nil {
		return nil, err
	}
	file, err := s.root.Open(key + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrCredentialNotFound
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Bound malformed records and avoid including their secret contents in errors.
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	var token oauth2.Token
	if len(data) > 1<<20 || json.Unmarshal(data, &token) != nil || token.AccessToken == "" {
		return nil, errors.New("invalid MCP credential record")
	}
	return &token, ctx.Err()
}

// Save publishes a complete token record atomically, preserving the old record on failure.
func (s *FileCredentialStore) Save(ctx context.Context, namespace, server string, token *oauth2.Token) error {
	// Validate before allocating a temporary file or replacing an existing credential.
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := credentialKey(namespace, server)
	if err != nil {
		return err
	}
	if token == nil || token.AccessToken == "" {
		return errors.New("MCP credential requires an access token")
	}
	data, err := json.Marshal(token)
	if err != nil {
		return errors.New("cannot encode MCP credential")
	}
	if len(data) > 1<<20 {
		return errors.New("MCP credential record exceeds size limit")
	}

	// A separate private staging file prevents partial tokens from becoming visible to readers.
	temporary := key + "." + oauth2.GenerateVerifier() + ".tmp"
	file, err := s.root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(temporary)
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr, ctx.Err()); err != nil {
		return err
	}
	if err := s.root.Rename(temporary, key+".json"); err != nil {
		return fmt.Errorf("publish MCP credential: %w", err)
	}
	return nil
}

// Delete forgets only the selected namespace/server credential; remote revocation is separate.
func (s *FileCredentialStore) Delete(ctx context.Context, namespace, server string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := credentialKey(namespace, server)
	if err != nil {
		return err
	}
	err = s.root.Remove(key + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
