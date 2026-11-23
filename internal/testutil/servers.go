package testutil

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

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

// RestateImage is the image StartRestate runs; it must already be present locally.
const RestateImage = "docker.restate.dev/restatedev/restate:latest"

// Restate is a Restate server in docker that calls back into deployments on this host.
type Restate struct {
	IngressURL string
	adminURL   string
}

// StartRestate runs RestateImage for the test, or skips when docker or the image is missing.
func StartRestate(t *testing.T) *Restate {
	t.Helper()
	if testing.Short() {
		t.Skip("restate tests are skipped in -short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if err := exec.Command("docker", "image", "inspect", RestateImage).Run(); err != nil {
		t.Skipf("restate image %s is not present locally", RestateImage)
	}
	ingress, admin := FreePort(t), FreePort(t)
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"-p", ingress+":8080", "-p", admin+":9070",
		"-e", "RUST_LOG=warn", RestateImage).Output()
	if err != nil {
		t.Skipf("docker run restate: %v", err)
	}
	container := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })

	r := &Restate{IngressURL: "http://127.0.0.1:" + ingress, adminURL: "http://127.0.0.1:" + admin}
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(r.adminURL + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("restate did not become healthy")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return r
}

// Register points Restate at a deployment serving on port of this host.
func (r *Restate) Register(t *testing.T, port string) {
	t.Helper()
	body := fmt.Sprintf(`{"uri":"http://host.docker.internal:%s","force":true}`, port)
	resp, err := http.Post(r.adminURL+"/deployments", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("register deployment: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("register deployment: status %d", resp.StatusCode)
	}
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
