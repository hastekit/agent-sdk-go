package daemon

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

type sandboxHandler func(http.ResponseWriter, *http.Request, string)

func withSandboxRoot(root string, h sandboxHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h(w, r, root) })
}
func resolvePath(root, rel string) (string, error) {
	cleanRoot := filepath.Clean(root)
	if strings.TrimSpace(rel) == "" {
		return cleanRoot, nil
	}
	target := filepath.Clean(rel)
	if !filepath.IsAbs(target) {
		target = filepath.Join(cleanRoot, target)
	}

	if target != cleanRoot && !strings.HasPrefix(target, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the sandbox workspace; use a path under %s", rel, cleanRoot)
	}
	return target, nil
}
