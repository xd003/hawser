package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Finsys/hawser/internal/config"
	"github.com/Finsys/hawser/internal/docker"
)

// fakeDockerCompose puts a `docker` on PATH whose compose run prints one line,
// waits for $GATE/release, then prints a second line.
func fakeDockerCompose(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1 $2\" = \"compose version\" ] && exit 0\necho building\nwhile [ ! -f \"$GATE/release\" ]; do sleep 0.01; done\necho done\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return t.TempDir()
}

func TestStandardComposeStreamsLinesBeforeCompletion(t *testing.T) {
	gate := fakeDockerCompose(t)
	srv := httptest.NewServer(http.HandlerFunc((&Server{cfg: &config.Config{}, compose: docker.NewComposeClient("/missing/docker.sock", "")}).handleCompose))
	defer srv.Close()
	body, _ := json.Marshal(docker.ComposeOperation{Operation: "up", ProjectName: "demo", ComposeFile: "services: {}", EnvVars: map[string]string{"GATE": gate}, StreamOutput: true})
	request, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	request.Header.Set(composeStreamHeader, "ndjson")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q", response.Header.Get("Content-Type"))
	}
	frames := make(chan composeStreamFrame)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			var frame composeStreamFrame
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				frames <- frame
			}
		}
	}()
	next := func() composeStreamFrame {
		t.Helper()
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("stream ended early")
			}
			return frame
		case <-time.After(5 * time.Second):
			t.Fatal("no frame while Compose was still running")
		}
		return composeStreamFrame{}
	}
	if frame := next(); frame.Type != "line" || frame.Line != "building" {
		t.Fatalf("first frame = %#v", frame)
	}
	if err := os.WriteFile(filepath.Join(gate, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if frame := next(); frame.Type != "line" || frame.Line != "done" {
		t.Fatalf("second frame = %#v", frame)
	}
	if frame := next(); frame.Type != "result" || frame.Status != http.StatusOK || frame.Result == nil || !frame.Result.Success {
		t.Fatalf("result frame = %#v", frame)
	}
}

// An older Dockhand sets streamOutput on every transport but cannot read NDJSON.
func TestStandardComposeWithoutStreamHeaderReturnsSingleJSON(t *testing.T) {
	gate := fakeDockerCompose(t)
	if err := os.WriteFile(filepath.Join(gate, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{}, compose: docker.NewComposeClient("/missing/docker.sock", "")}
	body, _ := json.Marshal(docker.ComposeOperation{Operation: "up", ProjectName: "demo", ComposeFile: "services: {}", EnvVars: map[string]string{"GATE": gate}, StreamOutput: true})
	rr := httptest.NewRecorder()
	server.handleCompose(rr, httptest.NewRequest(http.MethodPost, "/_hawser/compose", bytes.NewReader(body)))
	var result docker.ComposeResult
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/json" || json.Unmarshal(rr.Body.Bytes(), &result) != nil || result.Output != "building\ndone\n" {
		t.Fatalf("legacy response = %d %q %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
}
