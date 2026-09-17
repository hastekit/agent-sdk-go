package skills

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

// Exercise the real AWS SDK serialization and error decoding against an HTTP
// endpoint. No live AWS credentials or bucket are needed.
func TestS3ClientTransport(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		if r.URL.Query().Get("list-type") == "2" {
			prefix := r.URL.Query().Get("prefix")
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated>`)
			for key := range objects {
				if strings.HasPrefix(key, prefix) {
					fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", key)
				}
			}
			fmt.Fprint(w, "</ListBucketResult>")
			return
		}
		switch r.Method {
		case "PUT":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read", 500)
				return
			}
			objects[key] = data
			w.Header().Set("ETag", `"etag"`)
		case "GET":
			data, ok := objects[key]
			if !ok {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(404)
				fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
				return
			}
			w.Write(data)
		case "DELETE":
			delete(objects, key)
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	}))
	defer server.Close()
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}}, func(o *s3.Options) { o.BaseEndpoint = aws.String(server.URL); o.UsePathStyle = true })
	store, err := NewS3Store(client, S3Config{Bucket: "bucket", Prefix: "skills"})
	require.NoError(t, err)
	ctx := context.Background()
	want := bundle("review", "instructions")
	_, err = store.Put(ctx, "tenant", want)
	require.NoError(t, err)
	got, err := store.Get(ctx, "tenant", "review")
	require.NoError(t, err)
	require.Equal(t, want, got)
	page, err := store.List(ctx, "tenant", ListOptions{})
	require.NoError(t, err)
	require.Len(t, page.Skills, 1)
	_, err = store.Get(ctx, "other", "review")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, store.Delete(ctx, "tenant", "review"))
}
