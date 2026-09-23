package main

import (
	"testing"
	"time"
)

func TestConfigFromEnv(t *testing.T) {
	for _, key := range []string{"SANDBOX_PORT", "SANDBOX_ROOT", "SANDBOX_IDLE_TIMEOUT", "SANDBOX_SKILLS_DIR", "SANDBOX_SKILL_BIN_DIR"} {
		t.Setenv(key, "")
	}
	conf, err := configFromEnv()
	if err != nil || conf.Address != ":8080" || conf.RootDir != "/workspace" || conf.IdleTimeout != 30*time.Second {
		t.Fatalf("defaults: %+v, %v", conf, err)
	}
	t.Setenv("SANDBOX_IDLE_TIMEOUT", "0s")
	conf, err = configFromEnv()
	if err != nil || conf.IdleTimeout != 0 {
		t.Fatalf("disable idle: %+v, %v", conf, err)
	}
	for _, value := range []string{"bad", "-1s"} {
		t.Setenv("SANDBOX_IDLE_TIMEOUT", value)
		if _, err := configFromEnv(); err == nil {
			t.Fatalf("accepted idle timeout %q", value)
		}
	}
	t.Setenv("SANDBOX_IDLE_TIMEOUT", "30s")
	for _, value := range []string{"bad", "0", "65536"} {
		t.Setenv("SANDBOX_PORT", value)
		if _, err := configFromEnv(); err == nil {
			t.Fatalf("accepted port %q", value)
		}
	}
}
