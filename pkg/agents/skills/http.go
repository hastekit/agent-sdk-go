package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
)

// NewHandler exposes GET/POST /skills and GET/PUT/DELETE /skills/{name}.
// POST accepts multipart files (relative paths in Content-Disposition filename)
// or a JSON Bundle. PUT accepts JSON and checks the route name. Both replace the
// complete bundle. GET ?file=path returns a bundled file as an attachment.
// The host must wrap management endpoints with authentication/authorization.
func NewHandler(store Store, resolve func(*http.Request) (string, error)) http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, fn func(http.ResponseWriter, *http.Request, string)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			ns := "default"
			var err error
			if resolve != nil {
				ns, err = resolve(r)
				if err != nil {
					http.Error(w, "namespace denied", http.StatusForbidden)
					return
				}
			}
			if ns == "" {
				ns = "default"
			}
			if store == nil {
				http.Error(w, "skill store unavailable", http.StatusServiceUnavailable)
				return
			}
			fn(w, r, ns)
		})
	}
	route("GET /skills", func(w http.ResponseWriter, r *http.Request, ns string) {
		n := 0
		var err error
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err = strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				fail(w, fmt.Errorf("%w: limit must be 1–200", ErrInvalid))
				return
			}
		}
		page, err := store.List(r.Context(), ns, ListOptions{Limit: n, Cursor: r.URL.Query().Get("cursor")})
		if err != nil {
			fail(w, err)
			return
		}
		if page.Skills == nil {
			page.Skills = []Metadata{}
		}
		respond(w, page)
	})
	upload := func(w http.ResponseWriter, r *http.Request, ns string) {
		r.Body = http.MaxBytesReader(w, r.Body, 2*MaxBundleBytes)
		b, err := readUpload(r)
		if err != nil {
			fail(w, err)
			return
		}
		m, err := Validate(b)
		if err != nil {
			fail(w, err)
			return
		}
		if name := r.PathValue("name"); name != "" && name != m.Name {
			fail(w, fmt.Errorf("%w: route and skill name differ", ErrInvalid))
			return
		}
		m, err = store.Put(r.Context(), ns, b)
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, m)
	}
	route("POST /skills", upload)
	route("PUT /skills/{name}", upload)
	route("GET /skills/{name}", func(w http.ResponseWriter, r *http.Request, ns string) {
		b, err := store.Get(r.Context(), ns, r.PathValue("name"))
		if err != nil {
			fail(w, err)
			return
		}
		if r.URL.Query().Has("file") {
			file := r.URL.Query().Get("file")
			data, ok := b.Files[file]
			if !ok {
				fail(w, ErrNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file}))
			w.Write(data)
			return
		}
		respond(w, b)
	})
	route("DELETE /skills/{name}", func(w http.ResponseWriter, r *http.Request, ns string) {
		if err := store.Delete(r.Context(), ns, r.PathValue("name")); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
func readUpload(r *http.Request) (Bundle, error) {
	b := Bundle{Files: map[string][]byte{}}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return b, fmt.Errorf("%w: content type", ErrInvalid)
	}
	if media == "application/json" {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			return b, uploadError(err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return b, fmt.Errorf("%w: expected one JSON bundle", ErrInvalid)
		}
		return b, nil
	}
	if media != "multipart/form-data" {
		return b, fmt.Errorf("%w: expected JSON or multipart upload", ErrInvalid)
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return b, fmt.Errorf("%w: multipart body", ErrInvalid)
	}
	total := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return b, uploadError(err)
		}
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			part.Close()
			return b, ErrInvalid
		}
		name := params["filename"]
		if name == "" {
			part.Close()
			return b, fmt.Errorf("%w: every part must be a file", ErrInvalid)
		}
		if _, exists := b.Files[name]; exists {
			part.Close()
			return b, fmt.Errorf("%w: duplicate file %q", ErrInvalid, name)
		}
		if len(b.Files) >= MaxFiles {
			part.Close()
			return b, ErrTooLarge
		}
		data, err := io.ReadAll(io.LimitReader(part, int64(MaxBundleBytes-total+1)))
		part.Close()
		if err != nil {
			return b, uploadError(err)
		}
		total += len(data)
		if total > MaxBundleBytes {
			return b, ErrTooLarge
		}
		b.Files[name] = data
	}
	return b, nil
}
func uploadError(err error) error {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		return ErrTooLarge
	}
	return fmt.Errorf("%w: %v", ErrInvalid, err)
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "skill storage operation failed"
	switch {
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
		message = err.Error()
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
		message = err.Error()
	case errors.Is(err, ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
		message = err.Error()
	}
	http.Error(w, message, status)
}
