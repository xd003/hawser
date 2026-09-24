package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckStackDirAccess(t *testing.T) {
	dir := t.TempDir()
	if result := CheckStackDirAccess(&StackDirAccessRequest{SourceDir: dir}); !result.Success {
		t.Fatalf("accessible directory rejected: %s", result.Error)
	}
	if result := CheckStackDirAccess(&StackDirAccessRequest{SourceDir: filepath.Join(dir, "missing")}); result.Success {
		t.Fatal("missing directory reported as accessible")
	}
}

type adoptionRoundTripper func(*http.Request) (*http.Response, error)

func (f adoptionRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNormalizeRelativePath(t *testing.T) {
	for _, value := range []string{"", "/tmp/env", "../env", "a/../env", "a//env"} {
		if _, err := normalizeRelativePath(value); err == nil {
			t.Fatalf("normalizeRelativePath(%q) unexpectedly succeeded", value)
		}
	}
	for _, value := range []string{".env", "configs/compose.yaml"} {
		if got, err := normalizeRelativePath(value); err != nil || got != value {
			t.Fatalf("normalizeRelativePath(%q) = %q, %v", value, got, err)
		}
	}
}

func TestOverlayFilesPreservesSelectedEnvironment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("existing=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := overlayFiles(root, map[string]string{
		".env":         "git=true\n",
		"compose.yaml": "services:\n  app:\n    image: alpine\n",
	}, ".env", ""); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "existing=true\n" {
		t.Fatalf("preserved env = %q", env)
	}
	if _, err := os.Stat(filepath.Join(root, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTreeRemapsInternalAbsoluteSymlinks(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "target.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "target.txt"), filepath.Join(source, "link.txt")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(destination, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(destination, "target.txt") {
		t.Fatalf("remapped symlink = %q", target)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(destination, "link.txt")); err != nil || string(content) != "ok" {
		t.Fatalf("copied symlink after source removal = %q, %v", content, err)
	}
}

func TestRejectSpecialTreeRejectsExternalSymlinks(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := rejectSpecialTree(root); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("rejectSpecialTree error = %v", err)
	}
}

func TestAdoptionRollbackIsIdempotent(t *testing.T) {
	stacks := t.TempDir()
	source := t.TempDir()
	destination := filepath.Join(stacks, "project")
	if err := os.MkdirAll(destination, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "compose.yaml"), []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sourceID, err := fileIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	destinationID, err := fileIdentity(destination)
	if err != nil {
		t.Fatal(err)
	}
	j := adoptionJournal{
		AdoptionID:          "rollback-test",
		ProjectName:         "project",
		SourceDir:           source,
		DestinationDir:      destination,
		JournalDir:          filepath.Join(stacks, ".dockhand-adoptions", "rollback-test"),
		SourceIdentity:      sourceID,
		DestinationIdentity: destinationID,
		Phase:               "compose_succeeded",
	}
	if err := writeJournal(j); err != nil {
		t.Fatal(err)
	}

	client := NewComposeClient("", stacks)
	result, err := client.RollbackStackDirAdoption(&StackDirAdoptionIDRequest{AdoptionID: j.AdoptionID})
	if err != nil || !result.Success || result.Phase != "rolled_back" {
		t.Fatalf("rollback = %#v, %v", result, err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination still exists after rollback: %v", err)
	}

	result, err = client.RollbackStackDirAdoption(&StackDirAdoptionIDRequest{AdoptionID: j.AdoptionID})
	if err != nil || !result.Success || result.Phase != "rolled_back" {
		t.Fatalf("second rollback = %#v, %v", result, err)
	}
}

func TestAdoptionFinalizeRejectsReplacedSource(t *testing.T) {
	stacks := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	destination := filepath.Join(stacks, "project")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0755); err != nil {
		t.Fatal(err)
	}
	sourceID, err := fileIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	destinationID, err := fileIdentity(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.MkdirAll(replacement, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, source); err != nil {
		t.Fatal(err)
	}
	j := adoptionJournal{
		AdoptionID:          "finalize-test",
		ProjectName:         "project",
		SourceDir:           source,
		DestinationDir:      destination,
		JournalDir:          filepath.Join(stacks, ".dockhand-adoptions", "finalize-test"),
		SourceIdentity:      sourceID,
		DestinationIdentity: destinationID,
		Phase:               "compose_succeeded",
	}
	if err := writeJournal(j); err != nil {
		t.Fatal(err)
	}

	client := NewComposeClient("", stacks)
	result, err := client.FinalizeStackDirAdoption(&StackDirAdoptionIDRequest{AdoptionID: j.AdoptionID})
	if err != nil || result.Success || !strings.Contains(result.Error, "ownership changed") {
		t.Fatalf("finalize = %#v, %v", result, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("replacement source was removed: %v", err)
	}
}

func TestPrepareAdoptionPreservesEnvAndFinalizesSource(t *testing.T) {
	stacks := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(source, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".env"), []byte("existing=true\n"), 0600); err != nil {
		t.Fatal(err)
	}

	fakeDocker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nif [ \"$1\" = compose ] && [ \"$2\" = version ]; then exit 0; fi\nprintf 'compose ok\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fakeDocker)+string(os.PathListSeparator)+os.Getenv("PATH"))

	dockerClient := &Client{
		apiVersion: "v1.44",
		httpClient: &http.Client{Transport: adoptionRoundTripper(func(req *http.Request) (*http.Response, error) {
			body := `[{"Labels":{"com.docker.compose.project":"project","com.docker.compose.project.working_dir":"` + source + `","com.docker.compose.project.config_files":"` + composePath + `"}}]`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}
	client := NewComposeClient("", stacks)
	client.SetDockerClient(dockerClient)

	result, err := client.PrepareStackDirAdoption(context.Background(), &StackDirAdoptionRequest{
		AdoptionID:           "prepare-test",
		ProjectName:          "project",
		SourceDir:            source,
		SourceComposeFiles:   []string{composePath},
		SourceEnvPath:        filepath.Join(source, ".env"),
		PreservedEnvRelative: ".env",
		Compose: ComposeOperation{
			Operation:        "up",
			ProjectName:      "project",
			ComposeFileName:  "compose.yaml",
			ComposeFileNames: []string{"compose.yaml"},
			Files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: alpine\n",
				".env":         "git=true\n",
			},
		},
	}, nil)
	if err != nil || !result.Success {
		t.Fatalf("prepare = %#v, %v", result, err)
	}
	destination := filepath.Join(stacks, "project")
	env, err := os.ReadFile(filepath.Join(destination, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "existing=true\n" {
		t.Fatalf("managed env = %q", env)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source removed before finalize: %v", err)
	}

	result, err = client.FinalizeStackDirAdoption(&StackDirAdoptionIDRequest{AdoptionID: "prepare-test"})
	if err != nil || !result.Success || result.Phase != "finalized" {
		t.Fatalf("finalize = %#v, %v", result, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after finalize: %v", err)
	}
	result, err = client.FinalizeStackDirAdoption(&StackDirAdoptionIDRequest{AdoptionID: "prepare-test"})
	if err != nil || !result.Success || result.Phase != "finalized" {
		t.Fatalf("second finalize = %#v, %v", result, err)
	}
}

func TestEmptyManagedEnvContentIsReturned(t *testing.T) {
	empty := ""
	result := adoptionResult(adoptionJournal{ManagedEnvContent: &empty}, true, "")
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"managedEnvContent":""`) {
		t.Fatalf("empty managed env omitted: %s", data)
	}
}

func TestRecoveryKeepsSameDirectorySnapshot(t *testing.T) {
	stacks := t.TempDir()
	source := filepath.Join(stacks, "project")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "original"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	j := adoptionJournal{
		AdoptionID: "restart-test", ProjectName: "project", SourceDir: source,
		DestinationDir: source, SourceIdentity: identity, DestinationIdentity: identity,
		JournalDir:    filepath.Join(stacks, ".dockhand-adoptions", "restart-test"),
		SameDirectory: true, Phase: "compose_succeeded",
	}
	j.SnapshotDir = filepath.Join(j.JournalDir, "snapshot")
	if err := copyTreeWithin(source, j.SnapshotDir, source, source); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "original"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	client := NewComposeClient("", stacks)
	if err := client.RecoverStackDirAdoptions(); err != nil {
		t.Fatal(err)
	}
	result, err := client.RollbackStackDirAdoption(&StackDirAdoptionIDRequest{AdoptionID: j.AdoptionID})
	if err != nil || !result.Success {
		t.Fatalf("rollback after restart = %#v, %v", result, err)
	}
	content, err := os.ReadFile(filepath.Join(source, "original"))
	if err != nil || string(content) != "old" {
		t.Fatalf("restored content = %q, %v", content, err)
	}
}

func TestRecoveryRollsBackInterruptedPromotion(t *testing.T) {
	stacks := t.TempDir()
	source := t.TempDir()
	destination := filepath.Join(stacks, "project")
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	destinationID, err := fileIdentity(destination)
	if err != nil {
		t.Fatal(err)
	}
	j := adoptionJournal{
		AdoptionID: "promotion-test", ProjectName: "project", SourceDir: source,
		DestinationDir: destination, DestinationIdentity: destinationID,
		JournalDir: filepath.Join(stacks, ".dockhand-adoptions", "promotion-test"), Phase: "promoting",
	}
	if err := writeJournal(j); err != nil {
		t.Fatal(err)
	}
	client := NewComposeClient("", stacks)
	if err := client.RecoverStackDirAdoptions(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("interrupted destination remains: %v", err)
	}
	if result, err := client.RollbackStackDirAdoption(&StackDirAdoptionIDRequest{AdoptionID: j.AdoptionID}); err != nil || !result.Success {
		t.Fatalf("recovered journal = %#v, %v", result, err)
	}
}

func TestSnapshotAbsoluteSymlinkSurvivesRollback(t *testing.T) {
	stacks := t.TempDir()
	source := filepath.Join(stacks, "project")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	var err error
	source, err = canonicalExisting(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "target"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "target"), filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	j := adoptionJournal{
		AdoptionID: "symlink-test", ProjectName: "project", SourceDir: source,
		DestinationDir: source, SourceIdentity: identity, DestinationIdentity: identity,
		JournalDir:    filepath.Join(stacks, ".dockhand-adoptions", "symlink-test"),
		SameDirectory: true, Phase: "compose_succeeded",
	}
	j.SnapshotDir = filepath.Join(j.JournalDir, "snapshot")
	if err := copyTreeWithin(source, j.SnapshotDir, source, source); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(j); err != nil {
		t.Fatal(err)
	}
	client := NewComposeClient("", stacks)
	if result, err := client.RollbackStackDirAdoption(&StackDirAdoptionIDRequest{AdoptionID: j.AdoptionID}); err != nil || !result.Success {
		t.Fatalf("rollback = %#v, %v", result, err)
	}
	content, err := os.ReadFile(filepath.Join(source, "link"))
	if err != nil || string(content) != "ok" {
		t.Fatalf("restored absolute link = %q, %v", content, err)
	}
}
