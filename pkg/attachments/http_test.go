package attachments

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPUploadFetchAndAuthorization(t *testing.T) {
	store, err := NewFileStore(t.TempDir(), FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	// The host says whose request this is; here a header stands in for auth.
	handler := NewHTTPHandler(store, 1024, func(r *http.Request) (string, error) { return r.Header.Get("X-Namespace"), nil })
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, mime string
		data       []byte
	}{
		{"photo.png", "image/png", png}, {"report.pdf", "application/pdf", []byte("%PDF-1.7\nexample\n%%EOF")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			f, err := mw.CreateFormFile("file", tc.name)
			require.NoError(t, err)
			_, err = f.Write(tc.data)
			require.NoError(t, err)
			require.NoError(t, mw.Close())
			req := httptest.NewRequest("POST", "/attachments/", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			req.Header.Set("X-Namespace", "alice")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
			var file HTTPFile
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &file))
			require.Equal(t, tc.mime, file.MediaType)
			require.Equal(t, int64(len(tc.data)), file.Size)
			require.NotContains(t, rr.Body.String(), "base64")
			ref, err := RefFromURL(file.URL)
			require.NoError(t, err)
			require.Equal(t, file.FileID, FileID(ref))
			for _, namespace := range []string{"alice", "bob", ""} {
				get := httptest.NewRequest("GET", file.URL, nil)
				get.Header.Set("X-Namespace", namespace)
				fetched := httptest.NewRecorder()
				handler.ServeHTTP(fetched, get)
				if namespace == "alice" {
					require.Equal(t, 200, fetched.Code)
					require.Equal(t, tc.data, fetched.Body.Bytes())
					require.Equal(t, tc.mime, fetched.Header().Get("Content-Type"))
					require.Equal(t, "nosniff", fetched.Header().Get("X-Content-Type-Options"))
				} else {
					require.Contains(t, []int{403, 404}, fetched.Code)
					require.NotEqual(t, tc.data, fetched.Body.Bytes())
				}
			}
		})
	}
}

func TestHTTPUploadRejectsInvalidAndOversized(t *testing.T) {
	store, err := NewFileStore(t.TempDir(), FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	for _, tc := range []struct {
		data   string
		status int
	}{{"<html>not an image</html>", 400}, {"%PDF-1.7\n" + string(bytes.Repeat([]byte("x"), 100)), 413}} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		f, err := mw.CreateFormFile("file", "fake.png")
		require.NoError(t, err)
		_, err = io.WriteString(f, tc.data)
		require.NoError(t, err)
		require.NoError(t, mw.Close())
		req := httptest.NewRequest("POST", "/attachments/", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rr := httptest.NewRecorder()
		NewHTTPHandler(store, 64, func(*http.Request) (string, error) { return "test", nil }).ServeHTTP(rr, req)
		require.Equal(t, tc.status, rr.Code)
	}
	for _, raw := range []string{"https://evil/attachments/a", "//evil/attachments/a", "/attachments/../secret", "/attachments/a?tenant=b", "/attachments/a?version=1&version=2", "data:image/png;base64,AA"} {
		_, err := RefFromURL(raw)
		require.Error(t, err, raw)
	}
}
