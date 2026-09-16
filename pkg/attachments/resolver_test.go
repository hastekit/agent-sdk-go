package attachments

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testStore struct {
	loads   atomic.Int32
	lookups atomic.Int32
	denied  atomic.Bool
	gate    chan struct{}
	fail    atomic.Bool
}

func (s *testStore) Lookup(_ context.Context, namespace string, ref Ref) (Descriptor, error) {
	s.lookups.Add(1)
	if s.denied.Load() {
		return Descriptor{}, ErrDenied
	}
	return Descriptor{Namespace: namespace, Key: ref.ID, Version: ref.Version, Size: 3, MediaType: "text/plain"}, nil
}
func (s *testStore) Open(ctx context.Context, d Descriptor) (io.ReadCloser, error) {
	s.loads.Add(1)
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.fail.Load() {
		return nil, errors.New("storage unavailable")
	}
	return io.NopCloser(bytes.NewBufferString("abc")), nil
}
func TestCacheAuthorizationTenantVersionAndEviction(t *testing.T) {
	s := &testStore{}
	r := NewResolver(s, Config{CacheBytes: 3})
	ctx := context.Background()
	for range 2 {
		b, err := r.Resolve(ctx, "a", Ref{ID: "one"})
		require.NoError(t, err)
		var dst bytes.Buffer
		_, err = b.WriteTo(&dst)
		require.NoError(t, err)
		require.Equal(t, "abc", dst.String())
	}
	require.EqualValues(t, 1, s.loads.Load())
	require.EqualValues(t, 2, s.lookups.Load())
	s.denied.Store(true)
	_, err := r.Resolve(ctx, "a", Ref{ID: "one"})
	require.ErrorIs(t, err, ErrDenied)
	require.EqualValues(t, 1, s.loads.Load())
	s.denied.Store(false)
	_, err = r.Resolve(ctx, "b", Ref{ID: "one"})
	require.NoError(t, err)
	_, err = r.Resolve(ctx, "a", Ref{ID: "one"})
	require.NoError(t, err) // evicted by tenant b
	_, err = r.Resolve(ctx, "a", Ref{ID: "one", Version: "v2"})
	require.NoError(t, err)
	require.EqualValues(t, 4, s.loads.Load())
}
func TestConcurrentMissesShareLoadAndCancelledWaiterDoesNotPoisonIt(t *testing.T) {
	s := &testStore{gate: make(chan struct{})}
	r := NewResolver(s, Config{})
	ctx := context.Background()
	first, cancel := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() { _, err := r.Resolve(first, "a", Ref{ID: "x"}); firstDone <- err }()
	require.Eventually(t, func() bool { return s.loads.Load() == 1 }, time.Second, time.Millisecond)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := r.Resolve(ctx, "a", Ref{ID: "x"}); errs <- err }()
	}
	require.Eventually(t, func() bool { return s.lookups.Load() == 11 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-firstDone, context.Canceled)
	close(s.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, s.loads.Load())
}
func TestFailedLoadsAreNotCachedAndLimitsApplyBeforeOpen(t *testing.T) {
	s := &testStore{}
	s.fail.Store(true)
	r := NewResolver(s, Config{})
	ctx := context.Background()
	_, err := r.Resolve(ctx, "a", Ref{ID: "x"})
	require.Error(t, err)
	s.fail.Store(false)
	_, err = r.Resolve(ctx, "a", Ref{ID: "x"})
	require.NoError(t, err)
	require.EqualValues(t, 2, s.loads.Load())
	_, err = NewResolver(s, Config{MaxFileBytes: 2}).Resolve(ctx, "a", Ref{ID: "x"})
	require.ErrorIs(t, err, ErrTooLarge)
	require.EqualValues(t, 2, s.loads.Load())
}
