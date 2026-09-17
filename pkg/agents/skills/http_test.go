package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPUploadListReadIsolation(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	handler := NewHandler(store, func(r *http.Request) (string, error) { return r.Header.Get("Tenant"), nil })
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	b := bundle("review", "Instructions")
	for path, data := range b.Files {
		part, err := writer.CreateFormFile("files", path)
		require.NoError(t, err)
		_, err = part.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	req := httptest.NewRequest("POST", "/skills", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Tenant", "one")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	require.Equal(t, 200, resp.Code, resp.Body.String())
	for _, tc := range []struct {
		url, tenant string
		status      int
	}{
		{"/skills", "one", 200}, {"/skills/review", "one", 200}, {"/skills/review?file=refs/check.md", "one", 200}, {"/skills/review?file=../secret", "one", 404}, {"/skills/review?namespace=one", "two", 404}, {"/skills?limit=bad", "one", 400},
	} {
		req := httptest.NewRequest("GET", tc.url, nil)
		req.Header.Set("Tenant", tc.tenant)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		require.Equal(t, tc.status, resp.Code, resp.Body.String())
	}
	got, err := store.Get(context.Background(), "one", "review")
	require.NoError(t, err)
	require.Equal(t, b, got)
}
func TestHTTPValidationAndErrors(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	handler := NewHandler(store, nil)
	invalid := bundle("review", "content")
	invalid.Files["../escape"] = []byte("secret")
	encoded, _ := json.Marshal(invalid)
	req := httptest.NewRequest("POST", "/skills", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	require.Equal(t, 400, resp.Code)
	encoded, _ = json.Marshal(bundle("review", "content"))
	req = httptest.NewRequest("PUT", "/skills/different", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	require.Equal(t, 400, resp.Code)
	denied := NewHandler(store, func(*http.Request) (string, error) { return "", fmt.Errorf("denied") })
	resp = httptest.NewRecorder()
	denied.ServeHTTP(resp, httptest.NewRequest("GET", "/skills", nil))
	require.Equal(t, 403, resp.Code)
}

func TestHTTPRejectsOversizedAndDuplicateUploads(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	handler := NewHandler(store, nil)
	for _, duplicate := range []bool{false, true} {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		p, err := w.CreateFormFile("files", "SKILL.md")
		require.NoError(t, err)
		_, err = p.Write(bundle("review", "text").Files["SKILL.md"])
		require.NoError(t, err)
		name := "large.bin"
		if duplicate {
			name = "SKILL.md"
		}
		p, err = w.CreateFormFile("files", name)
		require.NoError(t, err)
		if duplicate {
			_, err = p.Write([]byte("duplicate"))
		} else {
			_, err = p.Write(make([]byte, MaxBundleBytes))
		}
		require.NoError(t, err)
		require.NoError(t, w.Close())
		req := httptest.NewRequest("POST", "/skills", &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if duplicate {
			require.Equal(t, 400, resp.Code)
		} else {
			require.Equal(t, 413, resp.Code)
		}
		_, err = store.Get(context.Background(), "default", "review")
		require.ErrorIs(t, err, ErrNotFound)
	}
}
