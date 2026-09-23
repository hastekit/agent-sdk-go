package daemon

import "testing"

func TestResolvePath(t *testing.T) {
	const root = "/workspace"

	tests := []struct {
		name    string
		rel     string
		want    string
		wantErr bool
	}{
		{name: "empty returns root", rel: "", want: root},
		{name: "whitespace returns root", rel: "  ", want: root},
		{name: "relative joins under root", rel: "tmp/jokes.txt", want: "/workspace/tmp/jokes.txt"},
		{name: "relative dot-slash joins under root", rel: "./tmp/jokes.txt", want: "/workspace/tmp/jokes.txt"},
		{name: "absolute inside root passes through", rel: "/workspace/tmp/jokes.txt", want: "/workspace/tmp/jokes.txt"},
		{name: "absolute root itself passes through", rel: "/workspace", want: root},
		{name: "absolute outside root rejected", rel: "/tmp/jokes.txt", wantErr: true},
		{name: "sibling prefix does not count as inside", rel: "/workspace-evil/x", wantErr: true},
		{name: "relative traversal out of root rejected", rel: "../other-session/x", wantErr: true},
		{name: "absolute traversal out of root rejected", rel: "/workspace/../etc/passwd", wantErr: true},
		{name: "traversal that stays inside is allowed", rel: "a/../b.txt", want: "/workspace/b.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePath(root, tt.rel)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolvePath(%q, %q) = %q, want error", root, tt.rel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePath(%q, %q) unexpected error: %v", root, tt.rel, err)
			}
			if got != tt.want {
				t.Fatalf("resolvePath(%q, %q) = %q, want %q", root, tt.rel, got, tt.want)
			}
		})
	}
}
