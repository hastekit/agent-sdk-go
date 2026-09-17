package attachments

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// URL returns the same-origin HTTP representation of a reference.
func URL(ref Ref) string {
	u := "/attachments/" + url.PathEscape(ref.ID)
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
	if err != nil {
		return Ref{}, ErrInvalid
	}
	for key, values := range q {
		if (key != "version" && key != "session_id") || len(values) != 1 {
			return Ref{}, ErrInvalid
		}
	}
	if thread := q.Get("session_id"); thread != "" && !validScopePart(thread) {
		return Ref{}, ErrInvalid
	}
	return Ref{ID: id, SessionID: q.Get("session_id"), Version: q.Get("version")}, nil
}

// InlineImageMediaType reports which formats may be previewed inline by the
// browser. It is not an upload or provider capability restriction.
func InlineImageMediaType(t string) bool {
	switch t {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// uploadMediaType preserves the declared file type, with filename and sniffing
// fallbacks for clients sending application/octet-stream. This only labels the
// original bytes; it never parses or converts document contents.
func uploadMediaType(filename, declared string, head []byte) string {
	detected := http.DetectContentType(head)
	if InlineImageMediaType(detected) || detected == "application/pdf" {
		return detected
	}
	media, _, _ := mime.ParseMediaType(declared)
	if media == "" || media == "application/octet-stream" {
		media, _, _ = mime.ParseMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))))
	}
	if media == "" || media == "application/octet-stream" {
		media, _, _ = mime.ParseMediaType(detected)
	}
	// Do not label non-image bytes as a raster image eligible for inline display.
	if InlineImageMediaType(media) {
		media, _, _ = mime.ParseMediaType(detected)
	}
	if media == "" {
		return "application/octet-stream"
	}
	return media
}

type HTTPFile struct {
	FileID           string `json:"file_id"`
	URL              string `json:"url"`
	Filename         string `json:"filename"`
	OriginalFilename string `json:"original_filename"`
	MediaType        string `json:"mediaType"`
	Size             int64  `json:"size"`
}

// NewHTTPHandler serves POST /attachments/ (multipart fields "file" and "session_id")
// and GET /attachments/{uuid}. Mount behind application authentication. UUID
// downloads require ReferenceStore and authorize within the supplied namespace.
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
		sessionValues := r.MultipartForm.Value["session_id"]
		if len(sessionValues) != 1 {
			fail(w, ErrInvalid)
			return
		}
		sessionID := sessionValues[0]
		if !validScopePart(sessionID) {
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
		mediaType := uploadMediaType(header.Filename, header.Header.Get("Content-Type"), head[:n])
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			fail(w, err)
			return
		}
		ref, err := store.Put(r.Context(), namespace, sessionID, Upload{Filename: header.Filename, MediaType: mediaType, Content: f})
		if err != nil {
			fail(w, err)
			return
		}
		d, err := store.Lookup(r.Context(), namespace, ref.SessionID, ref)
		if err != nil {
			fail(w, err)
			return
		}
		ref.Version = d.Version
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", URL(ref))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(HTTPFile{FileID: FileID(ref), URL: URL(ref), Filename: d.StoredFilename, OriginalFilename: d.Filename, MediaType: d.MediaType, Size: d.Size})
	})
	mux.HandleFunc("GET /attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
		namespace, err := namespaceOf(r)
		if err != nil {
			fail(w, err)
			return
		}
		ref := Ref{ID: r.PathValue("id"), SessionID: r.URL.Query().Get("session_id"), Version: r.URL.Query().Get("version")}
		var d Descriptor
		if ref.SessionID == "" {
			reader, ok := store.(ReferenceStore)
			if !ok {
				fail(w, ErrInvalid)
				return
			}
			d, err = reader.LookupReference(r.Context(), namespace, ref)
		} else {
			d, err = store.Lookup(r.Context(), namespace, ref.SessionID, ref)
		}
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
		if InlineImageMediaType(d.MediaType) {
			disposition = "inline"
		}
		w.Header().Set("Content-Type", d.MediaType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": d.StoredFilename}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, f)
		}
	})
	return mux
}
