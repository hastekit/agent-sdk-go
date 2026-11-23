package testutil

import (
	"context"
	"net"
	"os/exec"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
)

// StartTemporalDevServer runs the local temporal CLI's dev server for the test, or skips.
func StartTemporalDevServer(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("dev server tests are skipped in -short mode")
	}
	path, err := exec.LookPath("temporal")
	if err != nil {
		t.Skip("temporal CLI not on PATH")
	}
	srv, err := testsuite.StartDevServer(context.Background(), testsuite.DevServerOptions{
		ExistingPath:  path,
		LogLevel:      "error",
		ClientOptions: &client.Options{HostPort: "127.0.0.1:" + FreePort(t)},
	})
	if err != nil {
		t.Fatalf("start temporal dev server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv.FrontendHostPort()
}

// FreePort returns a TCP port free at the time of the call.
func FreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}
