package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestActiveRequestsPreventIdleShutdown(t *testing.T) {
	tracker := newIdleTracker()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	handler := withIdleTracking(tracker, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release }))
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v2/exec", nil))
		close(done)
	}()
	<-entered
	tracker.mu.Lock()
	tracker.lastRequestAt = time.Now().Add(-time.Hour)
	tracker.mu.Unlock()
	if tracker.idleDuration() != 0 {
		t.Error("active command considered idle")
	}
	close(release)
	<-done
	if tracker.idleDuration() > time.Second {
		t.Fatal("idle clock did not reset after completion")
	}
}
