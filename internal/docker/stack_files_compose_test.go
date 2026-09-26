package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundComposeRequiresRegisteredFilesBeforeRunningDocker(t *testing.T) {
	dir := t.TempDir()
	c := NewComposeClient("/missing/docker.sock", dir)
	if err := c.EnableStackFiles(); err != nil {
		t.Fatal(err)
	}
	result, err := c.Execute(context.Background(), &ComposeOperation{Operation: "up", ProjectName: "missing", BoundRoot: true}, nil)
	if err != nil || result.Success || !strings.Contains(result.Error, "no file-root binding") {
		t.Fatalf("unbound Compose ran: %#v %v", result, err)
	}
	stackFilesCall(t, c.stackFiles, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"nested/compose.yml"}}, 200)
	for _, op := range []ComposeOperation{
		{Operation: "up", ProjectName: "demo", BoundRoot: true, WorkDir: dir},
		{Operation: "up", ProjectName: "demo", BoundRoot: true, Files: map[string]string{"compose.yml": "unsafe"}},
		{Operation: "down", ProjectName: "demo", BoundRoot: true, RemoveFiles: true},
		{Operation: "up", ProjectName: "demo", BoundRoot: true, ComposeFileNames: []string{"wrong.yml"}},
		{Operation: "up", ProjectName: "demo", BoundRoot: true},
	} {
		result, err := c.Execute(context.Background(), &op, nil)
		if err != nil || result.Success || result.Error == "" {
			t.Fatalf("unsafe Compose operation permitted: %#v %v", op, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "demo", "nested")); !os.IsNotExist(err) {
		t.Fatalf("Compose staged a missing directory: %v", err)
	}

	update := &ComposeOperation{Operation: "up", ProjectName: "demo", BoundRoot: true, UpdateBoundComposeFiles: true, ComposeFileNames: []string{"nested/git.yml"}}
	result, err = c.Execute(context.Background(), update, nil)
	if err != nil || result.Success || !strings.Contains(result.Error, "nested/git.yml") {
		t.Fatalf("missing Git Compose file bypassed preflight: %#v %v", result, err)
	}
	binding := stackFilesCall(t, c.stackFiles, StackFileRequest{Action: "binding", ProjectName: "demo"}, 200)
	if len(binding.ComposeFileNames) != 1 || binding.ComposeFileNames[0] != "nested/compose.yml" {
		t.Fatalf("failed up changed binding: %#v", binding)
	}
}

func TestBoundComposeUsesOrderedFilesAndCommitsConversionAfterUp(t *testing.T) {
	dir := t.TempDir()
	client := NewComposeClient("/missing/docker.sock", dir)
	if err := client.EnableStackFiles(); err != nil {
		t.Fatal(err)
	}
	paths := []string{"service/base.yml", "service/override.yml"}
	stackFilesCall(t, client.stackFiles, StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: paths}, 200)
	for _, name := range []string{"service/base.yml", "service/override.yml", "service/git.yml", "service/.env", "service/.env.dockhand"} {
		stackFilesCall(t, client.stackFiles, StackFileRequest{Action: "write", ProjectName: "demo", Path: name, ContentBase64: "c2VydmljZXM6IHt9"}, 200)
	}
	script := filepath.Join(t.TempDir(), "compose")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ \"$SECRET_KEY\" = \"secret\" ] || exit 4\nprintf '%s\\n' \"$PWD\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client.composeChecked = true
	client.composeCmd = script
	client.composeArgs = nil
	op := &ComposeOperation{Operation: "up", ProjectName: "demo", BoundRoot: true, ComposeFileNames: paths, EnvVars: map[string]string{"SECRET_KEY": "secret"}}
	result, err := client.Execute(context.Background(), op, nil)
	if err != nil || !result.Success {
		t.Fatalf("bound Compose failed: %#v %v", result, err)
	}
	root, err := canonicalExisting(filepath.Join(dir, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		filepath.Join(root, "service"), "-p\ndemo\n",
		"-f\n" + filepath.Join(root, "service", "base.yml"),
		"-f\n" + filepath.Join(root, "service", "override.yml"),
		"--env-file\n" + filepath.Join(root, "service", ".env"),
		"--env-file\n" + filepath.Join(root, "service", ".env.dockhand"),
	} {
		if !strings.Contains(result.Output, required) {
			t.Fatalf("bound Compose lost cwd, ordering, or env precedence: %q missing %q", result.Output, required)
		}
	}
	convert := &ComposeOperation{Operation: "up", ProjectName: "demo", BoundRoot: true, ComposeFileNames: []string{"service/git.yml"}, UpdateBoundComposeFiles: true, EnvVars: map[string]string{"SECRET_KEY": "secret"}}
	result, err = client.Execute(context.Background(), convert, nil)
	if err != nil || !result.Success {
		t.Fatalf("conversion Compose failed: %#v %v", result, err)
	}
	binding := stackFilesCall(t, client.stackFiles, StackFileRequest{Action: "binding", ProjectName: "demo"}, 200)
	if len(binding.ComposeFileNames) != 1 || binding.ComposeFileNames[0] != "service/git.yml" {
		t.Fatalf("successful up did not commit ordered paths: %#v", binding)
	}
	client.composeCmd = "/bin/false"
	failure := &ComposeOperation{Operation: "up", ProjectName: "demo", BoundRoot: true, ComposeFileNames: paths, UpdateBoundComposeFiles: true}
	result, err = client.Execute(context.Background(), failure, nil)
	if err != nil || result.Success {
		t.Fatalf("failing Compose unexpectedly succeeded: %#v %v", result, err)
	}
	binding = stackFilesCall(t, client.stackFiles, StackFileRequest{Action: "binding", ProjectName: "demo"}, 200)
	if len(binding.ComposeFileNames) != 1 || binding.ComposeFileNames[0] != "service/git.yml" {
		t.Fatalf("failed up changed bound Compose paths: %#v", binding)
	}
}
