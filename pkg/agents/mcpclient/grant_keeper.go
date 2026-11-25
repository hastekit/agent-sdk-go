package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// grantKeeper keeps OAuthCredentialProvider's grants in a credential store and
// renews them. One grant's refreshes are serialized by its lock in this
// process, and across processes by the store when it is a CredentialUpdater.
type grantKeeper struct {
	store   CredentialStore
	client  *http.Client
	timeout time.Duration
	locks   sync.Map
}

// newGrantKeeper keeps grants in store, requesting tokens through client within
// timeout, 30 seconds when zero.
func newGrantKeeper(store CredentialStore, client *http.Client, timeout time.Duration) *grantKeeper {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &grantKeeper{store: store, client: client, timeout: timeout}
}

// credentialLock serializes refreshes and authorization writes for one stored
// grant in this process.
func (g *grantKeeper) credentialLock(namespace, key string) *sync.Mutex {
	id, _ := credentialKey(namespace, key)
	lock, _ := g.locks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// CredentialUpdater is implemented by a CredentialStore that can change one
// record atomically across processes, such as a database row read FOR UPDATE.
// OAuthCredentialProvider renews grants through it when the store has it, so
// two replicas never both redeem one refresh token: an authorization server
// that rotates refresh tokens may revoke the whole grant when one is used twice.
//
// UpdateCredential reads the namespace and key's record and, holding it against
// every other update of it, calls update with it, or with nil when there is
// none. What update returns is the record to keep: the token it was given keeps
// the record unchanged, another replaces it, and nil deletes it. When update
// returns an error the record stays as it was and UpdateCredential returns it.
type CredentialUpdater interface {
	UpdateCredential(ctx context.Context, namespace, key string, update func(*oauth2.Token) (*oauth2.Token, error)) error
}

// update changes the namespace's record under key as update says, through the
// store when it is a CredentialUpdater, else under the lock the caller holds.
func (g *grantKeeper) update(ctx context.Context, namespace, key string, update func(*oauth2.Token) (*oauth2.Token, error)) error {
	if updater, ok := g.store.(CredentialUpdater); ok {
		return updater.UpdateCredential(ctx, namespace, key, update)
	}
	current, err := g.store.Load(ctx, namespace, key)
	if errors.Is(err, ErrCredentialNotFound) {
		current, err = nil, nil
	}
	if err != nil {
		return err
	}
	next, err := update(current)
	switch {
	case err != nil:
		return err
	case next == nil && current != nil:
		return g.store.Delete(ctx, namespace, key)
	case next != nil && next != current:
		if err := g.store.Save(ctx, namespace, key, next); err != nil {
			return fmt.Errorf("persist refreshed MCP credential: %w", err)
		}
	}
	return nil
}

// renew returns a token of the namespace's grant to send: the stored one while
// fresh, else what refresh makes of it, read and written under the grant's lock
// and the store's own when it is a CredentialUpdater. A refresh returning nil
// deletes the grant. A missing grant is ErrCredentialNotFound.
func (g *grantKeeper) renew(ctx context.Context, namespace, key string, fresh func(*oauth2.Token) bool, refresh func(*oauth2.Token) (*oauth2.Token, error)) (*oauth2.Token, error) {
	// Most requests find a fresh token, and need no lock to send it.
	token, err := g.store.Load(ctx, namespace, key)
	if err != nil {
		return nil, err
	}
	if fresh(token) {
		return token, nil
	}

	lock := g.credentialLock(namespace, key)
	lock.Lock()
	defer lock.Unlock()

	// Read again under the lock: another request, or replica, may have just
	// refreshed the grant.
	var kept *oauth2.Token
	err = g.update(ctx, namespace, key, func(current *oauth2.Token) (*oauth2.Token, error) {
		if current == nil {
			return nil, ErrCredentialNotFound
		}
		if fresh(current) {
			kept = current
			return current, nil
		}
		next, err := refresh(current)
		kept = next
		return next, err
	})
	if err != nil {
		return nil, err
	}
	return kept, nil
}
