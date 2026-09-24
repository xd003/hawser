package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type HostFileEntry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	Type  string    `json:"type"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	Mode  string    `json:"mode"`
}

type HostFileListing struct {
	Path    string          `json:"path"`
	Parent  *string         `json:"parent"`
	Entries []HostFileEntry `json:"entries"`
}

func ListHostFiles(path string) (*HostFileListing, error) {
	if path == "" {
		path = "/"
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("path must be absolute")
	}
	path = filepath.Clean(path)
	items, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	listing := &HostFileListing{Path: path, Entries: []HostFileEntry{}}
	if path != "/" {
		parent := filepath.Dir(path)
		listing.Parent = &parent
	}
	for _, entry := range items {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		} else if !info.Mode().IsRegular() {
			continue
		}
		listing.Entries = append(listing.Entries, HostFileEntry{
			Name: entry.Name(), Path: filepath.Join(path, entry.Name()), Type: kind,
			Size: info.Size(), Mtime: info.ModTime(), Mode: fmt.Sprintf("%03o", info.Mode().Perm()),
		})
	}
	sort.Slice(listing.Entries, func(i, j int) bool {
		a, b := listing.Entries[i], listing.Entries[j]
		if a.Type != b.Type {
			return a.Type == "directory"
		}
		return a.Name < b.Name
	})
	return listing, nil
}
