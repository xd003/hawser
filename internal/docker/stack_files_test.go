package docker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func stackFilesCall(t *testing.T, s *StackFileService, req StackFileRequest, status int) StackFileResponse {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	code, body := s.Handle(context.Background(), payload)
	var response StackFileResponse
	if err = json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if code != status {
		t.Fatalf("%s %s: status %d, wanted %d (%s)", req.Action, req.Path, code, status, response.Error)
	}
	return response
}

func TestStackFilesBoundBinaryRevisionsAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStackFileService(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	project := "demo"
	stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: project, ComposeFileNames: []string{"nested/compose.yml", "nested/override.yml"}}, 200)
	bytes := []byte{0, 255, '\n', 'x'}
	encoded := base64.StdEncoding.EncodeToString(bytes)
	created := stackFilesCall(t, s, StackFileRequest{Action: "write", ProjectName: project, Path: "nested/compose.yml", ContentBase64: encoded}, 200)
	expected := sha256.Sum256(bytes)
	revision := hex.EncodeToString(expected[:])
	if created.Revision != revision {
		t.Fatalf("revision = %q", created.Revision)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "write", ProjectName: project, Path: "nested/compose.yml", ContentBase64: encoded}, 409)
	stackFilesCall(t, s, StackFileRequest{Action: "write", ProjectName: project, Path: "nested/compose.yml", ContentBase64: encoded, Revision: "stale"}, 409)
	s, err = NewStackFileService(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := stackFilesCall(t, s, StackFileRequest{Action: "binding", ProjectName: project}, 200)
	expectedRoot, err := canonicalExisting(filepath.Join(dir, project))
	if err != nil {
		t.Fatal(err)
	}
	if binding.Root != expectedRoot || len(binding.ComposeFileNames) != 2 || binding.ComposeFileNames[1] != "nested/override.yml" {
		t.Fatalf("lost ordered durable binding: %#v", binding)
	}
	read := stackFilesCall(t, s, StackFileRequest{Action: "read", ProjectName: project, Path: "nested/compose.yml"}, 200)
	if read.ContentBase64 != encoded || read.Revision != revision {
		t.Fatalf("binary bytes changed: %#v", read)
	}
	listing := stackFilesCall(t, s, StackFileRequest{Action: "list", ProjectName: project, Path: "nested"}, 200)
	if len(listing.Entries) != 1 || listing.Entries[0].Path != "nested/compose.yml" {
		t.Fatalf("unexpected listing: %#v", listing)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "move", ProjectName: project, Path: "nested/compose.yml", TargetPath: "nested/new.yml", Revision: revision}, 200)
	stackFilesCall(t, s, StackFileRequest{Action: "delete", ProjectName: project, Path: "nested/new.yml", Revision: revision}, 200)
	stackFilesCall(t, s, StackFileRequest{Action: "stat", ProjectName: project, Path: "nested/new.yml"}, 404)
}

func TestStackFilesRejectEscapeAndChangedRoot(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStackFileService(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml"}}, 200)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "demo")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", "/etc/passwd", "linked", ".dockhand-stack-bindings/demo.json", ".git/config", "foo/../secret"} {
		response := stackFilesCall(t, s, StackFileRequest{Action: "write", ProjectName: "demo", Path: path, ContentBase64: base64.StdEncoding.EncodeToString([]byte("bad"))}, 403)
		if response.Error == "" {
			t.Fatal("missing path rejection")
		}
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "safe" {
		t.Fatalf("outside file changed: %q %v", b, err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "binding", ProjectName: "demo"}, 409)
	stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml"}}, 409)
}

func TestStackFilesApplyGuardsRevisionsAndGitDeletions(t *testing.T) {
	s, err := NewStackFileService(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml"}}, 200)
	payload := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	create := func(path, text string) {
		t.Helper()
		stackFilesCall(t, s, StackFileRequest{Action: "write", ProjectName: "demo", Path: path, ContentBase64: payload(text)}, 200)
	}
	create("compose.yml", "services: {}")
	create("tracked", "v1")
	create("host-only", "keep")
	create("obsolete", "original")
	original := sha256.Sum256([]byte("original"))
	expected := hex.EncodeToString(original[:])
	// A stale batch never modifies an earlier valid file.
	stackFilesCall(t, s, StackFileRequest{Action: "apply", ProjectName: "demo", Files: []StackFileUpload{{Path: "new", ContentBase64: payload("new")}, {Path: "tracked", ContentBase64: payload("v2"), Revision: "wrong"}}}, 409)
	stackFilesCall(t, s, StackFileRequest{Action: "stat", ProjectName: "demo", Path: "new"}, 404)
	stackFilesCall(t, s, StackFileRequest{Action: "apply", ProjectName: "demo", Files: []StackFileUpload{{Path: "parent", ContentBase64: payload("file")}, {Path: "parent/child", ContentBase64: payload("impossible")}}}, 403)
	stackFilesCall(t, s, StackFileRequest{Action: "stat", ProjectName: "demo", Path: "parent"}, 404)
	current := sha256.Sum256([]byte("v1"))
	applied := stackFilesCall(t, s, StackFileRequest{Action: "apply", ProjectName: "demo", Files: []StackFileUpload{{Path: "tracked", ContentBase64: payload("v2"), Revision: hex.EncodeToString(current[:])}}, Deletions: []FileToDelete{{Path: "obsolete", Sha256: expected}, {Path: "host-only", Sha256: expected}, {Path: "compose.yml", Sha256: expected}}}, 200)
	if len(applied.DeletedFiles) != 1 || applied.DeletedFiles[0] != "obsolete" || len(applied.SkippedFiles) != 2 {
		t.Fatalf("unsafe deletion report: %#v", applied)
	}
	read := stackFilesCall(t, s, StackFileRequest{Action: "read", ProjectName: "demo", Path: "host-only"}, 200)
	if read.ContentBase64 != payload("keep") {
		t.Fatal("host-only file overwritten")
	}
	if !strings.Contains(stackFilesCall(t, s, StackFileRequest{Action: "read", ProjectName: "demo", Path: "compose.yml"}, 200).ContentBase64, payload("services: {}")) {
		t.Fatal("load-bearing file removed")
	}
}

func TestStackFilesMigrationExistingOnlyNeverSeeds(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStackFileService(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: "old", ComposeFileNames: []string{"compose.yml"}, ExistingOnly: true}, 404)
	if _, err := os.Stat(filepath.Join(dir, "old")); !os.IsNotExist(err) {
		t.Fatalf("migration created missing root: %v", err)
	}
}

func TestStackFilesEnrollmentChecksDockerOwnershipAndKeepsInPlace(t *testing.T) {
	managed := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "compose.yml"), []byte("services: {}"), 0644); err != nil {
		t.Fatal(err)
	}
	var project atomic.Value
	project.Store("someone-else")
	socket := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_ping":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/version":
			w.Write([]byte(`{"Version":"1","ApiVersion":"1.44"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]map[string]any{{"Labels": map[string]string{
				"com.docker.compose.project":              project.Load().(string),
				"com.docker.compose.project.working_dir":  external,
				"com.docker.compose.project.config_files": filepath.Join(external, "compose.yml"),
			}}})
		default:
			http.NotFound(w, r)
		}
	})}
	go httpServer.Serve(listener)
	t.Cleanup(func() { httpServer.Close() })
	dockerClient, err := NewClient(socket, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dockerClient.Close() })
	service, err := NewStackFileService(managed, dockerClient)
	if err != nil {
		t.Fatal(err)
	}
	request := StackFileRequest{Action: "enroll", ProjectName: "demo", Root: external, ComposeFileNames: []string{"compose.yml"}}
	stackFilesCall(t, service, request, 403)
	project.Store("demo")
	if err := os.WriteFile(filepath.Join(external, "override.yml"), []byte("services: {}"), 0644); err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, service, StackFileRequest{Action: "enroll", ProjectName: "demo", Root: external, ComposeFileNames: []string{"compose.yml", "override.yml"}}, 403)
	if err := os.Symlink("compose.yml", filepath.Join(external, "linked.yml")); err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, service, StackFileRequest{Action: "enroll", ProjectName: "demo", Root: external, ComposeFileNames: []string{"linked.yml"}}, 403)
	response := stackFilesCall(t, service, request, 200)
	resolved, err := canonicalExisting(external)
	if err != nil {
		t.Fatal(err)
	}
	if response.Root != resolved {
		t.Fatalf("enrollment moved source: %s", response.Root)
	}
	stackFilesCall(t, service, StackFileRequest{Action: "read", ProjectName: "demo", Path: "compose.yml"}, 200)
	if _, err := os.Stat(filepath.Join(managed, "demo")); !os.IsNotExist(err) {
		t.Fatalf("enrollment created managed staging: %v", err)
	}
}

func TestStackFilesManagedRelocationAndExclusiveBinding(t *testing.T) {
	dir := t.TempDir()
	service, err := NewStackFileService(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, service, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml"}}, 200)
	stackFilesCall(t, service, StackFileRequest{Action: "write", ProjectName: "demo", Path: "compose.yml", ContentBase64: base64.StdEncoding.EncodeToString([]byte("services: {}"))}, 200)
	managed, err := canonicalExisting(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(managed, "renamed")
	stackFilesCall(t, service, StackFileRequest{Action: "relocate", ProjectName: "demo", Root: filepath.Join(t.TempDir(), "outside")}, 403)
	relocated := stackFilesCall(t, service, StackFileRequest{Action: "relocate", ProjectName: "demo", Root: target}, 200)
	if relocated.Root != target {
		t.Fatalf("wrong relocated root: %s", relocated.Root)
	}
	stackFilesCall(t, service, StackFileRequest{Action: "read", ProjectName: "demo", Path: "compose.yml"}, 200)
	stackFilesCall(t, service, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml", "override.yml"}}, 200)
	if _, err := os.Stat(filepath.Join(managed, "demo")); !os.IsNotExist(err) {
		t.Fatalf("old managed directory still exists: %v", err)
	}
	stackFilesCall(t, service, StackFileRequest{Action: "bind", ProjectName: "renamed", ComposeFileNames: []string{"compose.yml"}}, 409)
}

func TestUnbindKeepsHostDataAndOnlyRemovesEmptyManagedRoot(t *testing.T) {
	dir := t.TempDir()
	service, err := NewStackFileService(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, service, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yaml"}}, 200)
	stackFilesCall(t, service, StackFileRequest{Action: "write", ProjectName: "demo", Path: "data/keep.db", ContentBase64: "ZGF0YQ=="}, 200)
	removed := stackFilesCall(t, service, StackFileRequest{Action: "unbind", ProjectName: "demo", RemoveEmptyRoot: true}, 200)
	if removed.RootRemoved {
		t.Fatal("unbind removed a directory that still holds host data")
	}
	if _, err := os.Stat(filepath.Join(dir, "demo", "data", "keep.db")); err != nil {
		t.Fatalf("host data was removed: %v", err)
	}
	stackFilesCall(t, service, StackFileRequest{Action: "binding", ProjectName: "demo"}, 404)
	// Idempotent, and a later re-create binds the existing directory again.
	stackFilesCall(t, service, StackFileRequest{Action: "unbind", ProjectName: "demo"}, 200)
	stackFilesCall(t, service, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yaml"}}, 200)
	if err := os.RemoveAll(filepath.Join(dir, "demo", "data")); err != nil {
		t.Fatal(err)
	}
	removed = stackFilesCall(t, service, StackFileRequest{Action: "unbind", ProjectName: "demo", RemoveEmptyRoot: true}, 200)
	if !removed.RootRemoved {
		t.Fatal("empty managed root was not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "demo")); !os.IsNotExist(err) {
		t.Fatalf("managed root still exists: %v", err)
	}
}
