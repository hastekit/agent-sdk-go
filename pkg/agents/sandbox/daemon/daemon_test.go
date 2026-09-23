package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunShutdown(t *testing.T) {
	for _, idle := range []bool{false, true} {
		name := "cancel"
		if idle {
			name = "idle"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := filepath.Join(t.TempDir(), "workspace")
			conf := Config{Address: "127.0.0.1:0", RootDir: root}
			if idle {
				conf.IdleTimeout = 10 * time.Millisecond
			} else {
				cancel()
			}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, conf) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("daemon did not stop")
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatal(err)
			}
		})
	}
}
