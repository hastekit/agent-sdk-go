package agui

import (
	"context"
	"net/http"
	"strings"
)

type namespaceContextKey struct{}

func requestNamespace(r *http.Request) string {
	if ns, ok := r.Context().Value(namespaceContextKey{}).(string); ok {
		return ns
	}
	return "default"
}

// Each route installs this wrapper after matching, before looking up agents,
// touching persistence, reading attachments or emitting streaming headers.
func (o options) withNamespace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns := "default"
		if o.namespaceResolver != nil {
			resolved, err := o.namespaceResolver(r)
			if err != nil {
				writeJSONError(w, http.StatusForbidden, "unable to resolve namespace")
				return
			}
			if strings.TrimSpace(resolved) != "" {
				ns = resolved
			}
		}
		ctx := context.WithValue(r.Context(), namespaceContextKey{}, ns)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
