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
	"time"
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

func TestStackFilesApplyFileModes(t *testing.T) {
	s, err := NewStackFileService(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	root := stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml"}}, 200).Root
	payload := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	revision := func(text string) string { sum := sha256.Sum256([]byte(text)); return hex.EncodeToString(sum[:]) }
	mode := func(m uint32) *uint32 { return &m }
	apply := func(status int, files ...StackFileUpload) {
		t.Helper()
		stackFilesCall(t, s, StackFileRequest{Action: "apply", ProjectName: "demo", Files: files}, status)
	}
	expectMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %#o, want %#o", path, got, want)
		}
	}

	// New files take the published mode.
	apply(200,
		StackFileUpload{Path: "start.sh", ContentBase64: payload("v1"), Mode: mode(0755)},
		StackFileUpload{Path: "config.txt", ContentBase64: payload("v1"), Mode: mode(0644)},
		StackFileUpload{Path: "legacy.sh", ContentBase64: payload("v1")},
	)
	expectMode("start.sh", 0755)
	expectMode("config.txt", 0644)
	expectMode("legacy.sh", 0644)

	// Existing files follow the published mode in both directions.
	apply(200,
		StackFileUpload{Path: "start.sh", ContentBase64: payload("v2"), Revision: revision("v1"), Mode: mode(0644)},
		StackFileUpload{Path: "config.txt", ContentBase64: payload("v2"), Revision: revision("v1"), Mode: mode(0755)},
	)
	expectMode("start.sh", 0644)
	expectMode("config.txt", 0755)

	// Without a mode an existing file keeps whatever the host gave it.
	if err := os.Chmod(filepath.Join(root, "legacy.sh"), 0750); err != nil {
		t.Fatal(err)
	}
	apply(200, StackFileUpload{Path: "legacy.sh", ContentBase64: payload("v2"), Revision: revision("v1")})
	expectMode("legacy.sh", 0750)

	// Env files stay private whatever mode is sent.
	apply(200,
		StackFileUpload{Path: ".env", ContentBase64: payload("A=1"), Mode: mode(0755)},
		StackFileUpload{Path: "nested/.env.dockhand", ContentBase64: payload("B=2"), Mode: mode(0644)},
	)
	expectMode(".env", 0600)
	expectMode("nested/.env.dockhand", 0600)

	// Any other mode rejects the whole batch before a file changes.
	for _, bad := range []uint32{0777, 04755, 0600, 0} {
		apply(http.StatusBadRequest,
			StackFileUpload{Path: "untouched", ContentBase64: payload("x"), Mode: mode(0755)},
			StackFileUpload{Path: "bad.sh", ContentBase64: payload("x"), Mode: mode(bad)},
		)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "stat", ProjectName: "demo", Path: "untouched"}, 404)
	stackFilesCall(t, s, StackFileRequest{Action: "stat", ProjectName: "demo", Path: "bad.sh"}, 404)
}

// A file above the default 10 MiB, the default 32 MiB apply cap, and (base64-
// encoded) the default 48 MiB request cap must round-trip once the per-file
// limit is raised, and be refused again at the default.
func TestStackFilesConfigurableSizeLimit(t *testing.T) {
	t.Cleanup(func() { ConfigureStackFileLimits(0) })
	s, err := NewStackFileService(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml"}}, 200)
	content := make([]byte, 40<<20)
	encoded := base64.StdEncoding.EncodeToString(content)
	upload := StackFileRequest{Action: "apply", ProjectName: "demo", Files: []StackFileUpload{{Path: "video.mp4", ContentBase64: encoded}}}

	ConfigureStackFileLimits(0)
	stackFilesCall(t, s, upload, http.StatusRequestEntityTooLarge)

	ConfigureStackFileLimits(40)
	stackFilesCall(t, s, upload, 200)
	read := stackFilesCall(t, s, StackFileRequest{Action: "read", ProjectName: "demo", Path: "video.mp4"}, 200)
	sum := sha256.Sum256(content)
	if read.Size != int64(len(content)) || read.Revision != hex.EncodeToString(sum[:]) {
		t.Fatalf("large file changed: size %d revision %s", read.Size, read.Revision)
	}

	ConfigureStackFileLimits(39)
	if response := stackFilesCall(t, s, StackFileRequest{Action: "read", ProjectName: "demo", Path: "video.mp4"}, 403); response.Error != "file exceeds 39 MiB: video.mp4" {
		t.Fatalf("unexpected rejection: %q", response.Error)
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

// A symlink in a stack directory is never served, but it must not fail the listing
// of everything else; a recursive delete then removes it with the hidden .git and
// never follows it out of the root.
func TestStackFilesListSkipsSymlinksAndRecursiveDeleteRemovesHiddenEntries(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStackFileService(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml"}}, 200)
	root := filepath.Join(dir, "demo")
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"app/.git", "app/src"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"app/.git/HEAD", "app/src/main.go", "app/README"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "app/current")); err != nil {
		t.Fatal(err)
	}

	listing := stackFilesCall(t, s, StackFileRequest{Action: "list", ProjectName: "demo", Path: "app"}, 200)
	var paths []string
	for _, e := range listing.Entries {
		paths = append(paths, e.Path)
	}
	if strings.Join(paths, ",") != "app/README,app/src" {
		t.Fatalf("listing = %v, want README and src only", paths)
	}

	// Without recursive the directory must be empty, as before.
	stackFilesCall(t, s, StackFileRequest{Action: "delete", ProjectName: "demo", Path: "app"}, 500)
	stackFilesCall(t, s, StackFileRequest{Action: "delete", ProjectName: "demo", Path: "", Recursive: true}, 403)
	stackFilesCall(t, s, StackFileRequest{Action: "delete", ProjectName: "demo", Path: "app/.git", Recursive: true}, 403)
	stackFilesCall(t, s, StackFileRequest{Action: "delete", ProjectName: "demo", Path: "app", Recursive: true}, 200)
	if _, err := os.Lstat(filepath.Join(root, "app")); !os.IsNotExist(err) {
		t.Fatalf("app still exists: %v", err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "safe" {
		t.Fatalf("symlink target changed: %q %v", b, err)
	}
}

// A long file action holds only its own project's lock: another project's
// requests are served meanwhile.
func TestStackFilesActionsLockPerProject(t *testing.T) {
	s, err := NewStackFileService(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one", "two"} {
		stackFilesCall(t, s, StackFileRequest{Action: "bind", ProjectName: p, ComposeFileNames: []string{"compose.yml"}}, 200)
	}
	held := s.fileLock("one")
	held.Lock()
	defer held.Unlock()
	served := make(chan struct{})
	go func() {
		defer close(served)
		stackFilesCall(t, s, StackFileRequest{Action: "write", ProjectName: "two", Path: "compose.yml", ContentBase64: base64.StdEncoding.EncodeToString([]byte("services: {}"))}, 200)
	}()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("project two blocked behind project one's file lock")
	}
}
