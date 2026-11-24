package docker

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// stubDockerSocket starts a minimal unix-socket Docker daemon that answers the
// two calls NewClient makes at construction (/_ping and /version), so the test
// can exercise the constructor without a real daemon. Returns the socket path.
func stubDockerSocket(t *testing.T) string {
	t.Helper()
	// A short path: the unix socket sun_path is capped (~104 bytes on macOS), and
	// go test's TempDir on macOS can exceed it. Create the socket directly in the
	// OS temp root with a short unique name.
	f, err := os.CreateTemp("", "hw*.sock")
	if err != nil {
		t.Fatalf("temp sock: %v", err)
	}
	sock := f.Name()
	_ = f.Close()
	_ = os.Remove(sock) // net.Listen wants the path free
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"29.0.0","ApiVersion":"1.44"}`))
	})
	// Slow endpoints answer only after slowDockerCall, like a prune or a
	// listing on saturated storage.
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(slowDockerCall)
		_, _ = w.Write([]byte(`{}`))
	}
	mux.HandleFunc("/images/prune", slow)
	mux.HandleFunc("/images/json", slow)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(sock) })
	return sock
}

// #104: REQUEST_TIMEOUT must reach the standard-mode socket client. A slow
// /containers/create (e.g. ZFS syncfs waiting on a txg commit) exceeds the old
// fixed 30s and strands the update; the timeout must be configurable.
func TestNewClient_HonorsRequestTimeout(t *testing.T) {
	sock := stubDockerSocket(t)
	c, err := NewClient(sock, 120)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.httpClient.Timeout; got != 120*time.Second {
		t.Fatalf("httpClient.Timeout = %v, want 120s", got)
	}
}

func TestNewClient_TimeoutFallsBackToDefault(t *testing.T) {
	sock := stubDockerSocket(t)
	for _, v := range []int{0, -5} {
		c, err := NewClient(sock, v)
		if err != nil {
			t.Fatalf("NewClient(%d): %v", v, err)
		}
		if got := c.httpClient.Timeout; got != 30*time.Second {
			t.Fatalf("NewClient(%d) timeout = %v, want 30s default", v, got)
		}
	}
}

const slowDockerCall = 1500 * time.Millisecond

// A prune answers only when Docker has finished it, which on slow storage
// takes far longer than REQUEST_TIMEOUT. Proxied prunes must get PruneTimeout
// while every other proxied call keeps REQUEST_TIMEOUT.
func TestRequestRaw_PruneOutlivesRequestTimeout(t *testing.T) {
	sock := stubDockerSocket(t)
	c, err := NewClient(sock, 1)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := c.RequestRaw(context.Background(), http.MethodPost, `/images/prune?filters={"dangling":["false"]}`, nil, nil)
	if err != nil {
		t.Fatalf("prune hit REQUEST_TIMEOUT: %v", err)
	}
	_ = resp.Body.Close()

	if resp, err := c.RequestRaw(context.Background(), http.MethodGet, "/images/json", nil, nil); err == nil {
		_ = resp.Body.Close()
		t.Fatal("non-prune call outlived REQUEST_TIMEOUT")
	}
}
