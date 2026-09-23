// sandbox-daemon serves the SDK execution and file protocol inside a sandbox.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/daemon"
)

func configFromEnv() (daemon.Config, error) {
	get := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	port := get("SANDBOX_PORT", "8080")
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return daemon.Config{}, fmt.Errorf("invalid SANDBOX_PORT %q", port)
	}
	idle, err := time.ParseDuration(get("SANDBOX_IDLE_TIMEOUT", "30s"))
	if err != nil || idle < 0 {
		return daemon.Config{}, fmt.Errorf("invalid SANDBOX_IDLE_TIMEOUT")
	}
	return daemon.Config{
		Address:     net.JoinHostPort("", port),
		RootDir:     get("SANDBOX_ROOT", "/workspace"),
		IdleTimeout: idle,
		SkillsDir:   get("SANDBOX_SKILLS_DIR", "/skills"),
		SkillBinDir: get("SANDBOX_SKILL_BIN_DIR", "/usr/local/bin"),
	}, nil
}

func main() {
	conf, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, conf); err != nil {
		log.Fatal(err)
	}
}
