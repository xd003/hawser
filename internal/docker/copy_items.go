package docker

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// copyStackItems copies host files without following symlinks. Git files are
// written afterwards, so repository-tracked paths retain their Git content.
func copyStackItems(paths []string, destination string) error {
	if len(paths) > 100 {
		return fmt.Errorf("too many selected items")
	}
	for _, source := range paths {
		if !filepath.IsAbs(source) || source == string(filepath.Separator) {
			return fmt.Errorf("select absolute file or directory paths")
		}
		source = filepath.Clean(source)
		rel, err := filepath.Rel(source, destination)
		if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return fmt.Errorf("cannot copy a directory into itself")
		}
		inside, err := filepath.Rel(destination, source)
		if err != nil || (inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator))) {
			return fmt.Errorf("item is already inside the destination")
		}
		output := filepath.Join(destination, filepath.Base(source))
		if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
				return fmt.Errorf("cannot copy symbolic links or special files: %s", path)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("cannot copy onto a symbolic link: %s", path)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(source, path)
			to := filepath.Join(output, rel)
			if entry.IsDir() {
				return os.MkdirAll(to, 0755)
			}
			from, err := os.Open(path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				from.Close()
				return err
			}
			dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
			if err != nil {
				from.Close()
				return err
			}
			_, copyErr := io.Copy(dst, from)
			from.Close()
			closeErr := dst.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}); err != nil {
			return err
		}
	}
	return nil
}
