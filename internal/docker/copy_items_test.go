package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyStackItems(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "managed")
	if err := os.MkdirAll(filepath.Join(source, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(source, "assets", "data.txt")
	if err := os.WriteFile(file, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := copyStackItems([]string{filepath.Join(source, "assets")}, destination); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, filepath.Join(destination, "assets", "data.txt")} {
		if content, err := os.ReadFile(path); err != nil || string(content) != "original" {
			t.Fatalf("%s: %s %v", path, content, err)
		}
	}
	if err := os.Symlink(file, filepath.Join(source, "assets", "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyStackItems([]string{filepath.Join(source, "assets")}, destination); err == nil {
		t.Fatal("accepted a symlink")
	}
	if err := copyStackItems([]string{source}, filepath.Join(source, "nested")); err == nil {
		t.Fatal("accepted recursive copy")
	}
}

func TestListHostFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	listing, err := ListHostFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 2 || listing.Entries[0].Type != "directory" || listing.Entries[1].Type != "file" {
		t.Fatalf("unexpected entries: %+v", listing.Entries)
	}
}
