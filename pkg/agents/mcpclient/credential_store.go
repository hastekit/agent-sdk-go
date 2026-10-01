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

// OAuthClient is the OAuth client and endpoints the MCP SDK's authorization flow
// resolved for a server whose endpoints are discovered: the client the config
// names, or one registered with the authorization server (RFC 7591). Refreshing
// the grant needs them, and a later authorization reuses the registration.
type OAuthClient struct {
	ClientID     string           `json:"clientId"`
	ClientSecret string           `json:"clientSecret,omitempty"`
	AuthURL      string           `json:"authUrl"`
	TokenURL     string           `json:"tokenUrl"`
	AuthStyle    oauth2.AuthStyle `json:"authStyle,omitempty"`
	Scopes       []string         `json:"scopes,omitempty"`
}

// OAuthClientStore is implemented by a CredentialStore that can also keep the
// OAuth client a grant was issued to, under the grant's own namespace and key.
// Servers whose OAuth endpoints are discovered need it; FileCredentialStore
// implements it. LoadOAuthClient returns ErrCredentialNotFound when there is none.
type OAuthClientStore interface {
	LoadOAuthClient(ctx context.Context, namespace, key string) (*OAuthClient, error)
	SaveOAuthClient(ctx context.Context, namespace, key string, client *OAuthClient) error
	DeleteOAuthClient(ctx context.Context, namespace, key string) error
}

var _ OAuthClientStore = (*FileCredentialStore)(nil)

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
	data, err := s.read(ctx, namespace, server, ".json")
	if err != nil {
		return nil, err
	}
	var token oauth2.Token
	if json.Unmarshal(data, &token) != nil || token.AccessToken == "" {
		return nil, errors.New("invalid MCP credential record")
	}
	return &token, ctx.Err()
}

// Save publishes a complete token record atomically, preserving the old record on failure.
func (s *FileCredentialStore) Save(ctx context.Context, namespace, server string, token *oauth2.Token) error {
	if token == nil || token.AccessToken == "" {
		return errors.New("MCP credential requires an access token")
	}
	data, err := json.Marshal(token)
	if err != nil {
		return errors.New("cannot encode MCP credential")
	}
	return s.write(ctx, namespace, server, ".json", data)
}

// Delete forgets only the selected namespace/server credential; remote revocation is separate.
func (s *FileCredentialStore) Delete(ctx context.Context, namespace, server string) error {
	return s.remove(ctx, namespace, server, ".json")
}

// LoadOAuthClient implements OAuthClientStore.
func (s *FileCredentialStore) LoadOAuthClient(ctx context.Context, namespace, server string) (*OAuthClient, error) {
	data, err := s.read(ctx, namespace, server, ".client.json")
	if err != nil {
		return nil, err
	}
	var client OAuthClient
	if json.Unmarshal(data, &client) != nil || client.ClientID == "" || client.TokenURL == "" {
		return nil, errors.New("invalid MCP OAuth client record")
	}
	return &client, ctx.Err()
}

// SaveOAuthClient implements OAuthClientStore.
func (s *FileCredentialStore) SaveOAuthClient(ctx context.Context, namespace, server string, client *OAuthClient) error {
	if client == nil || client.ClientID == "" || client.TokenURL == "" {
		return errors.New("MCP OAuth client requires a client ID and token URL")
	}
	data, err := json.Marshal(client)
	if err != nil {
		return errors.New("cannot encode MCP OAuth client")
	}
	return s.write(ctx, namespace, server, ".client.json", data)
}

// DeleteOAuthClient implements OAuthClientStore.
func (s *FileCredentialStore) DeleteOAuthClient(ctx context.Context, namespace, server string) error {
	return s.remove(ctx, namespace, server, ".client.json")
}

// read returns one record, bounded, without including its secret contents in errors.
func (s *FileCredentialStore) read(ctx context.Context, namespace, server, suffix string) ([]byte, error) {
	// Check cancellation and scope before touching any file.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := credentialKey(namespace, server)
	if err != nil {
		return nil, err
	}
	file, err := s.root.Open(key + suffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrCredentialNotFound
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("invalid MCP credential record")
	}
	return data, nil
}

// write publishes a complete record atomically, preserving the old record on failure.
func (s *FileCredentialStore) write(ctx context.Context, namespace, server, suffix string, data []byte) error {
	// Validate before allocating a temporary file or replacing an existing record.
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := credentialKey(namespace, server)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("MCP credential record exceeds size limit")
	}

	// A separate private staging file prevents partial records from becoming visible to readers.
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
	if err := s.root.Rename(temporary, key+suffix); err != nil {
		return fmt.Errorf("publish MCP credential: %w", err)
	}
	return nil
}

// remove forgets one record; a missing one is already forgotten.
func (s *FileCredentialStore) remove(ctx context.Context, namespace, server, suffix string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := credentialKey(namespace, server)
	if err != nil {
		return err
	}
	err = s.root.Remove(key + suffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
