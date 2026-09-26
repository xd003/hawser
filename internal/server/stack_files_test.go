package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Finsys/hawser/internal/config"
	"github.com/Finsys/hawser/internal/docker"
)

func TestStackFilesStandardRouteUsesBoundService(t *testing.T) {
	c := docker.NewComposeClient("/missing/docker.sock", t.TempDir())
	if err := c.EnableStackFiles(); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{Token: "test"}, compose: c}
	call := func(request any, want int) map[string]any {
		t.Helper()
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		server.handleStackFiles(rr, httptest.NewRequest(http.MethodPost, "/_hawser/stack-files", bytes.NewReader(data)))
		if rr.Code != want {
			t.Fatalf("status = %d, wanted %d: %s", rr.Code, want, rr.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	bound := call(map[string]any{"action": "bind", "projectName": "example", "composeFileNames": []string{"compose.yml"}}, 200)
	if bound["root"] == "" {
		t.Fatalf("missing bound root: %v", bound)
	}
	call(map[string]any{"action": "write", "projectName": "example", "path": "compose.yml", "contentBase64": base64.StdEncoding.EncodeToString([]byte("services: {}"))}, 200)
	read := call(map[string]any{"action": "read", "projectName": "example", "path": "compose.yml"}, 200)
	if read["contentBase64"] != base64.StdEncoding.EncodeToString([]byte("services: {}")) {
		t.Fatalf("read wrong bytes: %v", read)
	}
	call(map[string]any{"action": "read", "projectName": "example", "path": "../secret"}, 403)
}

func TestStackFilesStandardRequiresToken(t *testing.T) {
	c := docker.NewComposeClient("/missing/docker.sock", t.TempDir())
	if err := c.EnableStackFiles(); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{Token: "secret"}, compose: c, rateLimiter: NewAuthRateLimiter(10, time.Minute)}
	handler := server.authMiddleware(http.HandlerFunc(server.handleStackFiles))
	request := httptest.NewRequest(http.MethodPost, "/_hawser/stack-files", bytes.NewBufferString(`{"action":"bind","projectName":"demo","composeFileNames":["compose.yml"]}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated binding returned %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/_hawser/stack-files", bytes.NewBufferString(`{"action":"binding","projectName":"demo"}`))
	request.Header.Set("X-Hawser-Token", "secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated request created root: %d %s", response.Code, response.Body.String())
	}
}

func TestStackFilesRejectsUntokenedStandardMode(t *testing.T) {
	c := docker.NewComposeClient("/missing/docker.sock", t.TempDir())
	if err := c.EnableStackFiles(); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{}, compose: c}
	response := httptest.NewRecorder()
	server.handleStackFiles(response, httptest.NewRequest(http.MethodPost, "/_hawser/stack-files", bytes.NewBufferString(`{"action":"bind","projectName":"demo","composeFileNames":["compose.yml"]}`)))
	if response.Code != http.StatusUpgradeRequired {
		t.Fatalf("untokened write returned %d: %s", response.Code, response.Body.String())
	}
}
