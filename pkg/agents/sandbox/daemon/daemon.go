package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// idleTracker keeps track of the last request time to implement idle timeout
type idleTracker struct {
	mu            sync.Mutex
	lastRequestAt time.Time
	active        int
}

func newIdleTracker() *idleTracker {
	return &idleTracker{
		lastRequestAt: time.Now(),
	}
}

func (t *idleTracker) touch() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastRequestAt = time.Now()
}

func (t *idleTracker) idleDuration() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active > 0 {
		return 0
	}
	return time.Since(t.lastRequestAt)
}

// withIdleTracking is a middleware that updates the last request time on every request
func withIdleTracking(tracker *idleTracker, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracker.mu.Lock()
		tracker.active++
		tracker.lastRequestAt = time.Now()
		tracker.mu.Unlock()
		defer func() { tracker.mu.Lock(); tracker.active--; tracker.lastRequestAt = time.Now(); tracker.mu.Unlock() }()
		next.ServeHTTP(w, r)
	})
}

// setupSkillBinaries symlinks executables from /skills/*/bin into targetDir (e.g. /usr/local/bin)
// so they are available in the default PATH without modifying PATH.
func setupSkillBinaries(skillsDir, targetDir string) {
	skills, err := os.ReadDir(skillsDir)
	if err != nil {
		log.Printf("No skills directory found at %s: %v", skillsDir, err)
		return
	}

	for _, skill := range skills {
		if !skill.IsDir() {
			continue
		}
		binDir := filepath.Join(skillsDir, skill.Name(), "bin")
		entries, err := os.ReadDir(binDir)
		if err != nil {
			continue // no bin directory
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			src := filepath.Join(binDir, entry.Name())
			dst := filepath.Join(targetDir, entry.Name())
			if err := os.Symlink(src, dst); err != nil {
				log.Printf("Warning: failed to symlink %s -> %s: %v", src, dst, err)
			} else {
				log.Printf("Linked skill binary: %s", entry.Name())
			}
		}
	}
}

// Config configures a daemon running inside an isolated container or VM.
// The HTTP API intentionally has no authentication; expose it only on a trusted
// private network. RootDir scopes file APIs and the default command directory,
// not shell access: the container or VM provides the security boundary.
type Config struct {
	Address     string
	RootDir     string
	IdleTimeout time.Duration
	SkillsDir   string
	SkillBinDir string
}

// Run serves the SDK protocol until cancellation or idle timeout. A zero idle
// timeout disables idle shutdown. Config is independent of application globals.
func Run(ctx context.Context, conf Config) error {
	if conf.Address == "" {
		conf.Address = ":8080"
	}
	if conf.RootDir == "" {
		conf.RootDir = "/workspace"
	}
	if !filepath.IsAbs(conf.RootDir) {
		return fmt.Errorf("sandbox root must be absolute")
	}
	if conf.IdleTimeout < 0 {
		return fmt.Errorf("idle timeout must not be negative")
	}
	if err := os.MkdirAll(conf.RootDir, 0755); err != nil {
		return err
	}
	if conf.SkillsDir != "" && conf.SkillBinDir != "" {
		setupSkillBinaries(conf.SkillsDir, conf.SkillBinDir)
	}
	tracker := newIdleTracker()
	mux := http.NewServeMux()
	mux.Handle("/v2/exec", withSandboxRoot(conf.RootDir, handleExecV2))
	mux.Handle("/v2/files", withSandboxRoot(conf.RootDir, handleFilesV2))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	handler := otelhttp.NewHandler(withIdleTracking(tracker, mux), "SandboxDaemon", otelhttp.WithServerName("sandbox-daemon"))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{
		Addr: conf.Address, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return runCtx },
	}
	listener, err := net.Listen("tcp", conf.Address)
	if err != nil {
		return err
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ticks <-chan time.Time
		if conf.IdleTimeout > 0 {
			interval := min(conf.IdleTimeout, 5*time.Second)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			ticks = ticker.C
		}
		for {
			select {
			case <-runCtx.Done():
				_ = server.Close()
				return
			case <-ticks:
				if tracker.idleDuration() >= conf.IdleTimeout {
					log.Printf("sandbox-daemon idle timeout reached")
					cancel()
				}
			}
		}
	}()
	log.Printf("sandbox-daemon listening on %s (root=%s, idle_timeout=%v)", listener.Addr(), conf.RootDir, conf.IdleTimeout)
	err = server.Serve(listener)
	cancel()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
