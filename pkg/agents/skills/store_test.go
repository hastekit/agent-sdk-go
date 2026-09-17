package skills

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"
)

func bundle(name, body string) Bundle {
	return Bundle{Files: map[string][]byte{"SKILL.md": []byte("---\nname: " + name + "\ndescription: Review releases\n---\n" + body), "refs/check.md": []byte("check")}}
}

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeS3) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[aws.ToString(in.Key)] = data
	return &s3.PutObjectOutput{}, nil
}
func (f *fakeS3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data))}, nil
}
func (f *fakeS3) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}
func (f *fakeS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for key := range f.objects {
		if strings.HasPrefix(key, aws.ToString(in.Prefix)) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	start := 0
	if in.ContinuationToken != nil {
		start, _ = strconv.Atoi(*in.ContinuationToken)
	}
	end := min(start+int(aws.ToInt32(in.MaxKeys)), len(keys))
	out := &s3.ListObjectsV2Output{}
	for _, key := range keys[start:end] {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(key)})
	}
	if end < len(keys) {
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}
func stores(t *testing.T) map[string]Store {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	remote, err := NewS3Store(&fakeS3{objects: map[string][]byte{}}, S3Config{Bucket: "private", Prefix: "skills"})
	require.NoError(t, err)
	return map[string]Store{"filesystem": fs, "s3": remote}
}
func TestStoreContract(t *testing.T) {
	for kind, store := range stores(t) {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			for _, name := range []string{"a", "b", "c"} {
				_, err := store.Put(ctx, "tenant-a", bundle(name, "original"))
				require.NoError(t, err)
			}
			_, err := store.Get(ctx, "tenant-b", "a")
			require.ErrorIs(t, err, ErrNotFound)
			_, err = store.Get(ctx, "tenant-a", "../a")
			require.ErrorIs(t, err, ErrInvalid)
			var names []string
			cursor := ""
			for {
				page, err := store.List(ctx, "tenant-a", ListOptions{Limit: 1, Cursor: cursor})
				require.NoError(t, err)
				require.Len(t, page.Skills, 1)
				names = append(names, page.Skills[0].Name)
				cursor = page.NextCursor
				if cursor == "" {
					break
				}
			}
			require.ElementsMatch(t, []string{"a", "b", "c"}, names)
			replacement := bundle("a", "updated")
			delete(replacement.Files, "refs/check.md")
			_, err = store.Put(ctx, "tenant-a", replacement)
			require.NoError(t, err)
			got, err := store.Get(ctx, "tenant-a", "a")
			require.NoError(t, err)
			require.Equal(t, replacement, got)
			require.NoError(t, store.Delete(ctx, "tenant-a", "a"))
			require.NoError(t, store.Delete(ctx, "tenant-a", "a"))
			_, err = store.Get(ctx, "tenant-a", "a")
			require.ErrorIs(t, err, ErrNotFound)
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			_, err = store.List(cancelled, "tenant-a", ListOptions{})
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}
func TestStoreConcurrentReplacementIsAtomic(t *testing.T) {
	for kind, store := range stores(t) {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			_, err := store.Put(ctx, "tenant", bundle("a", "initial"))
			require.NoError(t, err)
			var wg sync.WaitGroup
			for i := range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := store.Put(ctx, "tenant", bundle("a", fmt.Sprint(i)))
					require.NoError(t, err)
					got, err := store.Get(ctx, "tenant", "a")
					require.NoError(t, err)
					_, err = Validate(got)
					require.NoError(t, err)
				}()
			}
			wg.Wait()
		})
	}
}
func TestInvalidBundles(t *testing.T) {
	bad := []Bundle{{}, bundle("../escape", "x"), bundle("ok", "x"), bundle("ok", "x"), bundle("ok", "x")}
	bad[2].Files["../secret"] = []byte("x")
	bad[3].Files["SKILL.md"] = []byte("no frontmatter")
	bad[4].Files["huge"] = make([]byte, MaxBundleBytes)
	for _, b := range bad {
		_, err := Validate(b)
		require.Error(t, err)
	}
}
func TestStoredSkillSetUsesNamespaceAndHostPolicy(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	_, err = store.Put(context.Background(), "tenant", bundle("review", "content"))
	require.NoError(t, err)
	set, err := NewSkillSet("library", store)
	require.NoError(t, err)
	empty, err := set.ListSkills(context.Background(), "default", nil)
	require.NoError(t, err)
	require.Empty(t, empty)
	rc := map[string]any{"user": "alice"}
	listed, err := set.ListSkills(context.Background(), "tenant", rc)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.False(t, listed[0].DefaultEnabled)
	required, err := NewSkillSet("library", store, WithGlobalNamespace("global"), WithRequiredSkills("review"))
	require.NoError(t, err)
	listed, err = required.ListSkills(context.Background(), "tenant", rc)
	require.NoError(t, err)
	require.False(t, listed[0].Required, "user-owned skills cannot be required")
	content, err := set.ResolveSkill(context.Background(), "tenant", rc, "review", "refs/check.md")
	require.NoError(t, err)
	require.Equal(t, "check", content)
	_, err = set.ResolveSkill(context.Background(), "tenant", rc, "review", "../secret")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFileStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	first, err := NewFileStore(dir)
	require.NoError(t, err)
	_, err = first.Put(context.Background(), "tenant", bundle("saved", "persistent"))
	require.NoError(t, err)
	second, err := NewFileStore(dir)
	require.NoError(t, err)
	got, err := second.Get(context.Background(), "tenant", "saved")
	require.NoError(t, err)
	require.Equal(t, bundle("saved", "persistent"), got)
}

func TestSkillSetUsesCallerNamespace(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	_, err = store.Put(ctx, "default", bundle("local", "default content"))
	require.NoError(t, err)
	_, err = store.Put(ctx, "global", bundle("builtin", "shared content"))
	require.NoError(t, err)
	source, err := NewSkillSet("user", store)
	require.NoError(t, err)
	listed, err := source.ListSkills(ctx, "", map[string]any{"Namespace": "global"})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "local", listed[0].Name)
	content, err := source.ResolveSkill(ctx, "", nil, "local", "")
	require.NoError(t, err)
	require.Contains(t, content, "default content")
	_, err = store.Put(ctx, "tenant", bundle("tenant-skill", "tenant content"))
	require.NoError(t, err)
	rc := map[string]any{"Namespace": "global"}
	listed, err = source.ListSkills(ctx, "tenant", rc)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "tenant-skill", listed[0].Name)
	content, err = source.ResolveSkill(ctx, "tenant", rc, "tenant-skill", "")
	require.NoError(t, err)
	require.Contains(t, content, "tenant content")
	_, err = source.ResolveSkill(ctx, "tenant", rc, "builtin", "")
	require.ErrorIs(t, err, ErrNotFound)
	listed, err = source.ListSkills(ctx, "global", nil)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "builtin", listed[0].Name)
	content, err = source.ResolveSkill(ctx, "global", nil, "builtin", "")
	require.NoError(t, err)
	require.Contains(t, content, "shared content")
}
