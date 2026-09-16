package attachments

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"
)

type storedS3Object struct {
	input s3.PutObjectInput
	data  string
	etag  string
}
type testS3 struct {
	objects map[string]storedS3Object
	puts    int
	err     error
}

func (m *testS3) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.puts++
	if m.err != nil {
		return nil, m.err
	}
	if aws.ToString(in.IfNoneMatch) != "*" {
		panic("missing create-only condition")
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	key := aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	if _, ok := m.objects[key]; ok {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	m.objects[key] = storedS3Object{*in, string(data), "\"etag\""}
	return &s3.PutObjectOutput{}, nil
}
func (m *testS3) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	o, ok := m.objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NotFound"}
	}
	return &s3.HeadObjectOutput{Metadata: o.input.Metadata, ContentType: o.input.ContentType, ContentLength: aws.Int64(int64(len(o.data))), ETag: aws.String(o.etag)}, nil
}
func (m *testS3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	o, ok := m.objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	if aws.ToString(in.IfMatch) != o.etag {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(o.data))}, nil
}
func TestS3StoreRoundTripAndIsolation(t *testing.T) {
	ctx := context.Background()
	api := &testS3{objects: map[string]storedS3Object{}}
	store, err := NewS3Store(api, S3StoreConfig{Bucket: "private", Prefix: "attachments/"})
	require.NoError(t, err)
	ref, err := store.Put(ctx, "tenant-a", Upload{Filename: "dir/résumé.txt", MediaType: "text/plain", Content: strings.NewReader("hello")})
	require.NoError(t, err)
	d, err := store.Lookup(ctx, "tenant-a", ref)
	require.NoError(t, err)
	require.Equal(t, "résumé.txt", d.Filename)
	require.EqualValues(t, 5, d.Size)
	require.Equal(t, ref.Version, d.SHA256)
	body, err := store.Open(ctx, d)
	require.NoError(t, err)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.Equal(t, "hello", string(data))
	_, err = store.Lookup(ctx, "tenant-b", ref)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Lookup(ctx, "", ref)
	require.ErrorIs(t, err, ErrDenied)
	_, err = store.Lookup(ctx, "tenant-a", Ref{ID: ref.ID, Version: "wrong"})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Lookup(ctx, "tenant-a", Ref{ID: "../escape"})
	require.ErrorIs(t, err, ErrInvalid)
	other, err := NewS3Store(api, S3StoreConfig{Bucket: "private"})
	require.NoError(t, err)
	_, err = other.Open(ctx, d)
	require.ErrorIs(t, err, ErrDenied)
	forged := d
	forged.Key = "outside/" + ref.ID
	_, err = store.Open(ctx, forged)
	require.ErrorIs(t, err, ErrDenied)
	// Resolver uses the same store and verifies the SHA-256 digest.
	_, err = NewResolver(store, Config{}).Resolve(ctx, "tenant-a", ref)
	require.NoError(t, err)
	key := "private/" + d.Key
	o := api.objects[key]
	o.etag = "changed"
	api.objects[key] = o
	_, err = store.Open(ctx, d)
	require.ErrorIs(t, err, ErrNotFound)
}
func TestS3StoreRejectsUploadsBeforeWriting(t *testing.T) {
	api := &testS3{objects: map[string]storedS3Object{}}
	store, err := NewS3Store(api, S3StoreConfig{Bucket: "private", MaxFileBytes: 4})
	require.NoError(t, err)
	for _, tc := range []struct {
		data, media string
		want        error
	}{
		{"12345", "text/plain", ErrTooLarge}, {"", "text/plain", ErrInvalid}, {"fake", "image/png", ErrInvalid}, {"x", "not a media type", ErrInvalid},
	} {
		_, err := store.Put(context.Background(), "tenant", Upload{MediaType: tc.media, Content: strings.NewReader(tc.data)})
		require.ErrorIs(t, err, tc.want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.Put(ctx, "tenant", Upload{MediaType: "text/plain", Content: strings.NewReader("ok")})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, api.puts)
	_, err = store.Put(context.Background(), "tenant", Upload{MediaType: "text/plain", Content: strings.NewReader("1234")})
	require.NoError(t, err)
	api.err = &smithy.GenericAPIError{Code: "AccessDenied"}
	_, err = store.Put(context.Background(), "tenant", Upload{MediaType: "text/plain", Content: strings.NewReader("ok")})
	require.ErrorIs(t, err, ErrDenied)
}

func TestS3StoreWithAWSClient(t *testing.T) {
	var data []byte
	var metadata http.Header
	var objectPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("missing conditional upload")
			}
			objectPath = r.URL.Path
			metadata = r.Header.Clone()
			var err error
			data, err = io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			w.Header().Set("ETag", `"test-etag"`)
		case http.MethodHead, http.MethodGet:
			if r.URL.Path != objectPath {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet && r.Header.Get("If-Match") != `"test-etag"` {
				t.Error("missing conditional download")
			}
			w.Header().Set("ETag", `"test-etag"`)
			w.Header().Set("Content-Type", metadata.Get("Content-Type"))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("X-Amz-Meta-Sha256", metadata.Get("X-Amz-Meta-Sha256"))
			w.Header().Set("X-Amz-Meta-Filename", metadata.Get("X-Amz-Meta-Filename"))
			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(server.URL)
		o.UsePathStyle = true
	})
	store, err := NewS3Store(client, S3StoreConfig{Bucket: "test-bucket", Prefix: "attachments"})
	require.NoError(t, err)
	ctx := context.Background()
	ref, err := store.Put(ctx, "tenant", Upload{Filename: "hello.txt", MediaType: "text/plain", Content: strings.NewReader("hello")})
	require.NoError(t, err)
	d, err := store.Lookup(ctx, "tenant", ref)
	require.NoError(t, err)
	body, err := store.Open(ctx, d)
	require.NoError(t, err)
	defer body.Close()
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "hello", string(got))
	require.Equal(t, "hello.txt", d.Filename)
}
