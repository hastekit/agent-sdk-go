package attachments

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// URL returns the same-origin HTTP representation of a reference.
func URL(ref Ref) string {
	u := "/attachments/" + url.PathEscape(ref.ID)
	if ref.Version != "" {
		u += "?version=" + url.QueryEscape(ref.Version)
	}
	return u
}

// RefFromURL accepts only this API's relative URLs. It never fetches URLs.
func RefFromURL(raw string) (Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/attachments/") {
		return Ref{}, ErrInvalid
	}
	id := strings.TrimPrefix(u.Path, "/attachments/")
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return Ref{}, ErrInvalid
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) > 1 || (len(q) == 1 && len(q["version"]) != 1) {
		return Ref{}, ErrInvalid
	}
	return Ref{ID: id, Version: q.Get("version")}, nil
}

// SupportedMediaType limits browser uploads to formats supported by the image
// and document request adapters. Other file types can still use Store directly.
func SupportedMediaType(t string) bool {
	switch t {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf":
		return true
	}
	return false
}

type HTTPFile struct {
	FileID    string `json:"file_id"`
	URL       string `json:"url"`
	Filename  string `json:"filename"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
}

// NewHTTPHandler serves POST /attachments/ (multipart field "file") and
// GET /attachments/{id}?version=... . Mount behind application authentication.
// namespaceOf reports the namespace a request may read and write, from whatever
// authenticated it; a request it cannot place is refused. maxBytes defaults to
// 20 MiB.
func NewHTTPHandler(store UploadStore, maxBytes int64, namespaceOf func(*http.Request) (string, error)) http.Handler {
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	if namespaceOf == nil {
		namespaceOf = func(*http.Request) (string, error) { return "", ErrDenied }
	}
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrInvalid):
			status = 400
		case errors.Is(err, ErrTooLarge):
			status = 413
		case errors.Is(err, ErrDenied):
			status = 403
		case errors.Is(err, ErrNotFound):
			status = 404
		}
		http.Error(w, http.StatusText(status), status)
	}
	mux.HandleFunc("POST /attachments/{$}", func(w http.ResponseWriter, r *http.Request) {
		namespace, err := namespaceOf(r)
		if err != nil {
			fail(w, err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				fail(w, ErrTooLarge)
			} else {
				fail(w, ErrInvalid)
			}
			return
		}
		defer r.MultipartForm.RemoveAll()
		if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
			fail(w, ErrInvalid)
			return
		}
		header := r.MultipartForm.File["file"][0]
		if header.Size > maxBytes {
			fail(w, ErrTooLarge)
			return
		}
		f, err := header.Open()
		if err != nil {
			fail(w, err)
			return
		}
		defer f.Close()
		head := make([]byte, 512)
		n, err := io.ReadFull(f, head)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			fail(w, err)
			return
		}
		mediaType := http.DetectContentType(head[:n])
		if !SupportedMediaType(mediaType) {
			fail(w, ErrInvalid)
			return
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			fail(w, err)
			return
		}
		ref, err := store.Put(r.Context(), namespace, Upload{Filename: header.Filename, MediaType: mediaType, Content: f})
		if err != nil {
			fail(w, err)
			return
		}
		d, err := store.Lookup(r.Context(), namespace, ref)
		if err != nil {
			fail(w, err)
			return
		}
		ref.Version = d.Version
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", URL(ref))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(HTTPFile{FileID: FileID(ref), URL: URL(ref), Filename: d.Filename, MediaType: d.MediaType, Size: d.Size})
	})
	mux.HandleFunc("GET /attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
		namespace, err := namespaceOf(r)
		if err != nil {
			fail(w, err)
			return
		}
		ref := Ref{ID: r.PathValue("id"), Version: r.URL.Query().Get("version")}
		d, err := store.Lookup(r.Context(), namespace, ref)
		if err != nil {
			fail(w, err)
			return
		}
		f, err := store.Open(r.Context(), d)
		if err != nil {
			fail(w, err)
			return
		}
		defer f.Close()
		disposition := "attachment"
		if strings.HasPrefix(d.MediaType, "image/") && SupportedMediaType(d.MediaType) {
			disposition = "inline"
		}
		w.Header().Set("Content-Type", d.MediaType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": d.Filename}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, f)
		}
	})
	return mux
}
