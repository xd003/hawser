package docker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const maxStackFileSize = 10 << 20
const maxStackApplyFiles = 256
const maxStackApplyBytes = 32 << 20
const maxStackRequestBytes = 48 << 20

// StackFileRequest is the identical JSON contract used by Standard and Edge.
// All file paths are slash-relative to the enrolled root; only enroll accepts
// an absolute path, after Docker's Compose ownership labels have been checked.
type StackFileRequest struct {
	Action           string            `json:"action"`
	ProjectName      string            `json:"projectName"`
	Path             string            `json:"path,omitempty"`
	TargetPath       string            `json:"targetPath,omitempty"`
	Root             string            `json:"root,omitempty"`
	ComposeFileNames []string          `json:"composeFileNames,omitempty"`
	ExistingOnly     bool              `json:"existingOnly,omitempty"`
	ContentBase64    string            `json:"contentBase64,omitempty"`
	Revision         string            `json:"revision,omitempty"`
	Files            []StackFileUpload `json:"files,omitempty"`
	Deletions        []FileToDelete    `json:"deletions,omitempty"`
	RemoveEmptyRoot  bool              `json:"removeEmptyRoot,omitempty"`
}

type StackFileUpload struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"contentBase64"`
	Revision      string `json:"revision,omitempty"`
}

type StackFileEntry struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	Size     int64  `json:"size,omitempty"`
	Revision string `json:"revision,omitempty"`
}

type StackFileResponse struct {
	Root             string           `json:"root,omitempty"`
	Managed          bool             `json:"managed,omitempty"`
	RootRemoved      bool             `json:"rootRemoved,omitempty"`
	ComposeFileNames []string         `json:"composeFileNames,omitempty"`
	Entry            *StackFileEntry  `json:"entry,omitempty"`
	Entries          []StackFileEntry `json:"entries,omitempty"`
	ContentBase64    string           `json:"contentBase64"`
	Revision         string           `json:"revision,omitempty"`
	Size             int64            `json:"size"`
	DeletedFiles     []string         `json:"deletedFiles,omitempty"`
	SkippedFiles     []SkippedFile    `json:"skippedFiles,omitempty"`
	Error            string           `json:"error,omitempty"`
}

type stackFileBinding struct {
	Root             string           `json:"root"`
	Identity         adoptionIdentity `json:"identity"`
	ComposeFileNames []string         `json:"composeFileNames"`
	Managed          bool             `json:"managed"`
}

type StackFileService struct {
	stacksDir    string
	registry     string
	dockerClient *Client
	// ponytail: one lock serializes all projects; use keyed locks if concurrent
	// large project applies become a measurable bottleneck.
	mu sync.Mutex
}

type stackFileError struct {
	status  int
	message string
}

func (e *stackFileError) Error() string { return e.message }
func fileError(status int, format string, args ...any) error {
	return &stackFileError{status, fmt.Sprintf(format, args...)}
}

func NewStackFileService(stacksDir string, client *Client) (*StackFileService, error) {
	if stacksDir == "" {
		return nil, fmt.Errorf("STACKS_DIR is not configured")
	}
	if err := os.MkdirAll(stacksDir, 0700); err != nil {
		return nil, err
	}
	managed, err := canonicalExisting(stacksDir)
	if err != nil {
		return nil, err
	}
	registry := filepath.Join(managed, ".dockhand-stack-bindings")
	if err := os.Mkdir(registry, 0700); err != nil {
		if !os.IsExist(err) {
			return nil, err
		}
	} else if err := syncDirectory(managed); err != nil {
		return nil, err
	}
	info, err := os.Lstat(registry)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || int(owner.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("stack binding registry must be an owner-only directory owned by Hawser")
	}
	return &StackFileService{stacksDir: managed, registry: registry, dockerClient: client}, nil
}

func stackIdentity(info os.FileInfo) (adoptionIdentity, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return adoptionIdentity{}, fmt.Errorf("directory identity unavailable")
	}
	return adoptionIdentity{Device: uint64(s.Dev), Inode: uint64(s.Ino)}, nil
}

func (s *StackFileService) bindingPath(project string) string {
	return filepath.Join(s.registry, project+".json")
}
func (s *StackFileService) load(project string) (stackFileBinding, error) {
	var b stackFileBinding
	data, err := os.ReadFile(s.bindingPath(project))
	if os.IsNotExist(err) {
		return b, fileError(http.StatusNotFound, "stack %s has no file-root binding", project)
	}
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(data, &b); err != nil {
		return b, fileError(http.StatusConflict, "invalid stack root binding: %v", err)
	}
	if b.Root == "" || len(b.ComposeFileNames) == 0 {
		return b, fileError(http.StatusConflict, "incomplete stack root binding")
	}
	return b, nil
}

func (s *StackFileService) openBound(project string) (stackFileBinding, *os.Root, error) {
	b, err := s.load(project)
	if err != nil {
		return b, nil, err
	}
	info, err := os.Lstat(b.Root)
	canonical, resolveErr := canonicalExisting(b.Root)
	if resolveErr != nil || canonical != b.Root {
		return b, nil, fileError(http.StatusConflict, "bound stack root canonical path changed: %s", b.Root)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return b, nil, fileError(http.StatusConflict, "bound stack root is missing or inaccessible: %s", b.Root)
	}
	id, err := stackIdentity(info)
	if err != nil || id != b.Identity {
		return b, nil, fileError(http.StatusConflict, "bound stack root identity changed: %s", b.Root)
	}
	root, err := os.OpenRoot(b.Root)
	if err != nil {
		return b, nil, fileError(http.StatusConflict, "cannot open bound stack root %s: %v", b.Root, err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return b, nil, fileError(http.StatusConflict, "bound stack root changed while opening: %s", b.Root)
	}
	return b, root, nil
}

func validateFileRel(p string) error {
	if !isSafeRelPath(p) {
		return fileError(http.StatusForbidden, "unsafe stack-relative path: %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".dockhand-stack-bindings" || part == ".dockhand-adoptions" || part == ".git" {
			return fileError(http.StatusForbidden, "protected stack path: %q", p)
		}
	}
	return nil
}

func safeEntry(root *os.Root, p string, allowMissing, directory bool) (os.FileInfo, error) {
	if p == "" {
		if !directory {
			return nil, fileError(http.StatusForbidden, "file path is required")
		}
		return root.Stat(".")
	}
	if err := validateFileRel(p); err != nil {
		return nil, err
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(name)
		if os.IsNotExist(err) && allowMissing && i == len(parts)-1 {
			return nil, nil
		}
		if os.IsNotExist(err) {
			return nil, fileError(http.StatusNotFound, "stack path not found: %s", name)
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil, fileError(http.StatusForbidden, "symlink or special file is forbidden: %s", name)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, fileError(http.StatusForbidden, "non-directory parent: %s", name)
		}
		if i == len(parts)-1 && directory && !info.IsDir() {
			return nil, fileError(http.StatusForbidden, "not a directory: %s", p)
		}
	}
	return root.Lstat(p)
}

func fileRevision(root *os.Root, p string) (string, int64, error) {
	info, err := safeEntry(root, p, false, false)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fileError(http.StatusForbidden, "not a regular file: %s", p)
	}
	if info.Size() > maxStackFileSize {
		return "", 0, fileError(http.StatusForbidden, "file exceeds 10 MiB: %s", p)
	}
	f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		return "", 0, fileError(http.StatusConflict, "file changed while opening: %s", p)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxStackFileSize+1))
	if err != nil {
		return "", 0, err
	}
	if n > maxStackFileSize {
		return "", 0, fileError(http.StatusForbidden, "file exceeds 10 MiB: %s", p)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func makeEntry(root *os.Root, p string) (*StackFileEntry, error) {
	info, err := safeEntry(root, p, false, false)
	if err != nil {
		return nil, err
	}
	entry := &StackFileEntry{Path: p, Type: "directory"}
	if info.Mode().IsRegular() {
		entry.Type = "file"
		entry.Size = info.Size()
		if info.Size() <= maxStackFileSize {
			entry.Revision, entry.Size, err = fileRevision(root, p)
		}
	}
	return entry, err
}

func validateComposeNames(names []string) error {
	if len(names) == 0 || len(names) > 32 {
		return fileError(http.StatusForbidden, "at least one and at most 32 Compose paths required")
	}
	seen := map[string]bool{}
	for _, p := range names {
		if err := validateFileRel(p); err != nil {
			return err
		}
		if seen[p] {
			return fileError(http.StatusForbidden, "duplicate Compose path: %s", p)
		}
		seen[p] = true
	}
	return nil
}

func (s *StackFileService) save(project string, b stackFileBinding) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.registry, ".binding-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp.Name(), s.bindingPath(project)); err != nil {
		return err
	}
	dir, err := os.Open(s.registry)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *StackFileService) verifyEnroll(ctx context.Context, project, source string, names []string) error {
	if s.dockerClient == nil {
		return fileError(http.StatusForbidden, "Docker ownership validation unavailable")
	}
	resp, err := s.dockerClient.Request(ctx, http.MethodGet, "/containers/json?all=true", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fileError(http.StatusForbidden, "Docker container inspection failed: %d", resp.StatusCode)
	}
	var containers []struct {
		Labels map[string]string `json:"Labels"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&containers); err != nil {
		return err
	}
	enrolledRoot, err := os.OpenRoot(source)
	if err != nil {
		return fileError(http.StatusForbidden, "Compose root cannot be opened inside Hawser: %v", err)
	}
	defer enrolledRoot.Close()
	expected := make([]string, len(names))
	for i, name := range names {
		info, err := safeEntry(enrolledRoot, name, false, false)
		if err != nil || !info.Mode().IsRegular() {
			return fileError(http.StatusForbidden, "Compose config file is not a regular accessible file: %s", name)
		}
		p, err := canonicalExisting(filepath.Join(source, filepath.FromSlash(name)))
		if err != nil || !pathInside(p, source) {
			return fileError(http.StatusForbidden, "Compose config file is unavailable inside Hawser: %s", name)
		}
		expected[i] = p
	}
	found := false
	for _, c := range containers {
		if c.Labels["com.docker.compose.project"] != project {
			continue
		}
		found = true
		work, err := canonicalExisting(c.Labels["com.docker.compose.project.working_dir"])
		if err != nil || work != source {
			return fileError(http.StatusForbidden, "Compose project working directory does not match accessible root")
		}
		labels := strings.Split(c.Labels["com.docker.compose.project.config_files"], ",")
		if len(labels) != len(expected) {
			return fileError(http.StatusForbidden, "Compose project config files do not match requested files")
		}
		for i, label := range labels {
			p, err := canonicalExisting(strings.TrimSpace(label))
			if err != nil || p != expected[i] {
				return fileError(http.StatusForbidden, "Compose project ordered config files do not match requested files")
			}
		}
	}
	if !found {
		return fileError(http.StatusForbidden, "no running or stopped Compose project owns the requested directory")
	}
	return nil
}

func (s *StackFileService) ensureExclusive(project, root string) error {
	entries, err := os.ReadDir(s.registry)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || name == project+".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.registry, name))
		if err != nil {
			return err
		}
		var other stackFileBinding
		if err := json.Unmarshal(data, &other); err != nil {
			return fileError(http.StatusConflict, "another stack binding is invalid: %v", err)
		}
		if other.Root == root || pathInside(other.Root, root) || pathInside(root, other.Root) {
			return fileError(http.StatusConflict, "stack root overlaps another project binding")
		}
	}
	return nil
}

func (s *StackFileService) bind(ctx context.Context, r StackFileRequest) (*StackFileResponse, error) {
	if err := validateComposeNames(r.ComposeFileNames); err != nil {
		return nil, err
	}
	var root string
	if r.Action == "bind" {
		if r.Root != "" {
			return nil, fileError(http.StatusForbidden, "managed binding does not accept an absolute root")
		}
		root = filepath.Join(s.stacksDir, r.ProjectName)
		if current, err := s.load(r.ProjectName); err == nil {
			if !current.Managed || filepath.Dir(current.Root) != s.stacksDir {
				return nil, fileError(http.StatusConflict, "project is already enrolled at an external root")
			}
			root = current.Root
		} else {
			var missing *stackFileError
			if !errors.As(err, &missing) || missing.status != http.StatusNotFound {
				return nil, err
			}
		}
		info, err := os.Lstat(root)
		if os.IsNotExist(err) && !r.ExistingOnly {
			err = os.Mkdir(root, 0755)
			if err == nil {
				err = syncDirectory(s.stacksDir)
			}
			if err == nil {
				info, err = os.Lstat(root)
			}
		}
		if os.IsNotExist(err) {
			return nil, fileError(http.StatusNotFound, "managed stack root does not exist: %s", root)
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fileError(http.StatusForbidden, "managed stack root is not a real directory")
		}
	} else {
		if !filepath.IsAbs(r.Root) {
			return nil, fileError(http.StatusForbidden, "enrollment requires an absolute accessible Compose root")
		}
		var err error
		root, err = canonicalExisting(r.Root)
		if err != nil {
			return nil, fileError(http.StatusNotFound, "Compose root is not accessible inside Hawser: %v; mount it into the agent", err)
		}
		if root == string(filepath.Separator) || root == s.stacksDir || pathInside(s.stacksDir, root) || pathInside(root, s.registry) || pathInside(root, filepath.Join(s.stacksDir, ".dockhand-adoptions")) || root == "/var" || root == "/home" || root == "/data" || root == "/tmp" {
			return nil, fileError(http.StatusForbidden, "protected or overlapping root cannot be enrolled")
		}
		for _, protected := range []string{"/etc", "/proc", "/sys", "/dev", "/run", "/var/run"} {
			if root == protected || pathInside(root, protected) {
				return nil, fileError(http.StatusForbidden, "protected agent or host root cannot be enrolled")
			}
		}
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fileError(http.StatusForbidden, "Compose root is not a real directory")
		}
		if err := s.verifyEnroll(ctx, r.ProjectName, root, r.ComposeFileNames); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	id, err := stackIdentity(info)
	if err != nil {
		return nil, err
	}
	if err := s.ensureExclusive(r.ProjectName, root); err != nil {
		return nil, err
	}
	b := stackFileBinding{Root: root, Identity: id, ComposeFileNames: r.ComposeFileNames, Managed: r.Action == "bind"}
	old, err := s.load(r.ProjectName)
	if err == nil {
		if old.Root != root || old.Identity != id {
			return nil, fileError(http.StatusConflict, "stack root already bound elsewhere or its directory identity changed")
		}
	} else {
		var f *stackFileError
		if !errors.As(err, &f) || f.status != http.StatusNotFound {
			return nil, err
		}
	}
	if err = s.save(r.ProjectName, b); err != nil {
		return nil, err
	}
	return &StackFileResponse{Root: root, ComposeFileNames: b.ComposeFileNames}, nil
}

// relocate moves a managed stack directory to another direct STACKS_DIR leaf.
// A retry after a crash between rename and registry commit recognizes the same
// directory identity at the destination and finishes the binding update.
func (s *StackFileService) relocate(r StackFileRequest) (*StackFileResponse, error) {
	binding, err := s.load(r.ProjectName)
	if err != nil {
		return nil, err
	}
	if !binding.Managed || filepath.Dir(binding.Root) != s.stacksDir {
		return nil, fileError(http.StatusForbidden, "external adopted roots cannot be relocated by Hawser")
	}
	if !filepath.IsAbs(r.Root) || filepath.Clean(r.Root) != r.Root ||
		filepath.Dir(r.Root) != s.stacksDir || !safeAdoptionName.MatchString(filepath.Base(r.Root)) {
		return nil, fileError(http.StatusForbidden, "destination must be a new direct STACKS_DIR leaf")
	}
	if r.Root == binding.Root {
		return &StackFileResponse{Root: binding.Root, ComposeFileNames: binding.ComposeFileNames}, nil
	}
	if err := s.ensureExclusive(r.ProjectName, r.Root); err != nil {
		return nil, err
	}
	targetInfo, targetErr := os.Lstat(r.Root)
	moved := false
	if targetErr == nil {
		targetID, err := stackIdentity(targetInfo)
		if err != nil || !targetInfo.IsDir() || targetInfo.Mode()&os.ModeSymlink != 0 || targetID != binding.Identity {
			return nil, fileError(http.StatusConflict, "destination already exists")
		}
		if _, err := os.Lstat(binding.Root); err == nil {
			return nil, fileError(http.StatusConflict, "source and destination both exist")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	} else if !os.IsNotExist(targetErr) {
		return nil, targetErr
	} else {
		info, err := os.Lstat(binding.Root)
		if err != nil {
			return nil, fileError(http.StatusConflict, "bound source directory is inaccessible: %v", err)
		}
		identity, err := stackIdentity(info)
		if err != nil || !info.IsDir() || identity != binding.Identity {
			return nil, fileError(http.StatusConflict, "bound source directory identity changed")
		}
		if err := os.Rename(binding.Root, r.Root); err != nil {
			return nil, err
		}
		moved = true
	}
	if err := syncDirectory(s.stacksDir); err != nil {
		if moved {
			_ = os.Rename(r.Root, binding.Root)
		}
		return nil, err
	}
	previous := binding.Root
	binding.Root = r.Root
	if err := s.save(r.ProjectName, binding); err != nil {
		if moved {
			if rollbackErr := os.Rename(r.Root, previous); rollbackErr != nil {
				return nil, fileError(http.StatusConflict, "relocation registry commit failed and rollback failed: %v; retry relocation to recover: %v", err, rollbackErr)
			}
		}
		return nil, err
	}
	return &StackFileResponse{Root: r.Root, ComposeFileNames: binding.ComposeFileNames}, nil
}

// unbind forgets a project's root binding after the stack is removed. It never
// deletes files; with removeEmptyRoot a managed STACKS_DIR leaf is removed only
// when it is still the bound directory and already empty.
func (s *StackFileService) unbind(r StackFileRequest) (*StackFileResponse, error) {
	b, err := s.load(r.ProjectName)
	if err != nil {
		var missing *stackFileError
		if errors.As(err, &missing) && missing.status == http.StatusNotFound {
			return &StackFileResponse{}, nil
		}
		return nil, err
	}
	result := &StackFileResponse{Root: b.Root, Managed: b.Managed}
	if r.RemoveEmptyRoot && b.Managed && filepath.Dir(b.Root) == s.stacksDir {
		if info, statErr := os.Lstat(b.Root); statErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if id, idErr := stackIdentity(info); idErr == nil && id == b.Identity {
				// os.Remove refuses a non-empty directory, so host data survives.
				if os.Remove(b.Root) == nil {
					result.RootRemoved = true
					if err := syncDirectory(s.stacksDir); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if err := os.Remove(s.bindingPath(r.ProjectName)); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := syncDirectory(s.registry); err != nil {
		return nil, err
	}
	return result, nil
}

func makeParents(root *os.Root, p string) error {
	parts := strings.Split(p, "/")
	for i := range len(parts) - 1 {
		dir := strings.Join(parts[:i+1], "/")
		info, err := safeEntry(root, dir, true, true)
		if err != nil {
			return err
		}
		if info == nil {
			if err = root.Mkdir(dir, 0755); err != nil {
				return err
			}
		}
	}
	return nil
}
func revisionGuard(root *os.Root, path, revision string, createParents bool) error {
	if err := validateFileRel(path); err != nil {
		return err
	}
	if createParents {
		if err := makeParents(root, path); err != nil {
			return err
		}
	}
	info, err := safeEntry(root, path, true, false)
	if err != nil {
		return err
	}
	if info == nil {
		if revision != "" {
			return fileError(http.StatusConflict, "file disappeared since revision %s: %s", revision, path)
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fileError(http.StatusForbidden, "not a regular file: %s", path)
	}
	if revision == "" {
		return fileError(http.StatusConflict, "file exists; provide its revision: %s", path)
	}
	current, _, err := fileRevision(root, path)
	if err != nil {
		return err
	}
	if current != revision {
		return fileError(http.StatusConflict, "stale file revision: %s", path)
	}
	return nil
}
func preflightUpload(root *os.Root, path, revision string) error {
	parts := strings.Split(path, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			if revision != "" {
				return fileError(http.StatusConflict, "file disappeared since revision: %s", path)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fileError(http.StatusForbidden, "symlink or special file is forbidden: %s", name)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fileError(http.StatusForbidden, "non-directory parent: %s", name)
		}
	}
	return revisionGuard(root, path, revision, false)
}
func atomicFile(root *os.Root, p string, data []byte) error {
	mode := os.FileMode(0644)
	secret := filepath.Base(p) == ".env" || filepath.Base(p) == ".env.dockhand"
	if secret {
		mode = 0600
	}
	if info, err := root.Lstat(p); err == nil && info.Mode().IsRegular() && !secret {
		mode = info.Mode().Perm()
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	temp := filepath.ToSlash(filepath.Join(filepath.Dir(p), ".dockhand-write-"+hex.EncodeToString(b[:])))
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := root.Rename(temp, p); err != nil {
		return err
	}
	dir := filepath.ToSlash(filepath.Dir(p))
	folder, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer folder.Close()
	return folder.Sync()
}
func decodeStackFile(s string) ([]byte, error) {
	if base64.StdEncoding.DecodedLen(len(s)) > maxStackFileSize+2 {
		return nil, fileError(http.StatusForbidden, "file exceeds 10 MiB")
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fileError(http.StatusBadRequest, "invalid base64 file content")
	}
	if len(b) > maxStackFileSize {
		return nil, fileError(http.StatusForbidden, "file exceeds 10 MiB")
	}
	return b, nil
}

func (s *StackFileService) action(ctx context.Context, r StackFileRequest) (*StackFileResponse, error) {
	if len(r.ProjectName) > 128 || !safeAdoptionName.MatchString(r.ProjectName) {
		return nil, fileError(http.StatusForbidden, "invalid Compose project name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Action == "bind" || r.Action == "enroll" {
		return s.bind(ctx, r)
	}
	if r.Action == "relocate" {
		return s.relocate(r)
	}
	if r.Action == "unbind" {
		return s.unbind(r)
	}
	b, root, err := s.openBound(r.ProjectName)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	switch r.Action {
	case "binding":
		return &StackFileResponse{Root: b.Root, Managed: b.Managed, ComposeFileNames: b.ComposeFileNames}, nil
	case "stat":
		e, err := makeEntry(root, r.Path)
		return &StackFileResponse{Entry: e}, err
	case "list":
		if _, err := safeEntry(root, r.Path, false, true); err != nil {
			return nil, err
		}
		dir := r.Path
		if dir == "" {
			dir = "."
		}
		f, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil || !opened.IsDir() {
			return nil, fileError(http.StatusConflict, "directory changed while opening: %s", dir)
		}
		children, err := f.ReadDir(2001)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(children) > 2000 {
			return nil, fileError(http.StatusForbidden, "stack directory has more than 2000 entries")
		}
		result := &StackFileResponse{Entries: []StackFileEntry{}}
		for _, child := range children {
			path := child.Name()
			if r.Path != "" {
				path = r.Path + "/" + path
			}
			if err := validateFileRel(path); err != nil {
				continue
			}
			entry, err := makeEntry(root, path)
			if err != nil {
				return nil, err
			}
			result.Entries = append(result.Entries, *entry)
		}
		return result, nil
	case "read":
		info, err := safeEntry(root, r.Path, false, false)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fileError(http.StatusForbidden, "not a regular file: %s", r.Path)
		}
		if info.Size() > maxStackFileSize {
			return nil, fileError(http.StatusForbidden, "file exceeds 10 MiB: %s", r.Path)
		}
		f, err := root.OpenFile(r.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			return nil, fileError(http.StatusConflict, "file changed while opening: %s", r.Path)
		}
		data, err := io.ReadAll(io.LimitReader(f, maxStackFileSize+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxStackFileSize {
			return nil, fileError(http.StatusForbidden, "file exceeds 10 MiB")
		}
		sum := sha256.Sum256(data)
		return &StackFileResponse{ContentBase64: base64.StdEncoding.EncodeToString(data), Revision: hex.EncodeToString(sum[:]), Size: int64(len(data))}, nil
	case "mkdir":
		if err := validateFileRel(r.Path); err != nil {
			return nil, err
		}
		if err := makeParents(root, r.Path); err != nil {
			return nil, err
		}
		if info, err := safeEntry(root, r.Path, true, true); err != nil {
			return nil, err
		} else if info != nil {
			return nil, fileError(http.StatusConflict, "directory already exists: %s", r.Path)
		}
		if err := root.Mkdir(r.Path, 0755); err != nil {
			return nil, err
		}
		e, err := makeEntry(root, r.Path)
		return &StackFileResponse{Entry: e}, err
	case "write":
		data, err := decodeStackFile(r.ContentBase64)
		if err != nil {
			return nil, err
		}
		if err := revisionGuard(root, r.Path, r.Revision, true); err != nil {
			return nil, err
		}
		if err := atomicFile(root, r.Path, data); err != nil {
			return nil, err
		}
		e, err := makeEntry(root, r.Path)
		return &StackFileResponse{Entry: e, Revision: e.Revision}, err
	case "move":
		if err := validateFileRel(r.TargetPath); err != nil {
			return nil, err
		}
		if r.TargetPath == r.Path || strings.HasPrefix(r.TargetPath, r.Path+"/") {
			return nil, fileError(http.StatusForbidden, "cannot move a stack item into itself")
		}
		info, err := safeEntry(root, r.Path, false, false)
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			if err = revisionGuard(root, r.Path, r.Revision, false); err != nil {
				return nil, err
			}
		} else if r.Revision != "" {
			return nil, fileError(http.StatusConflict, "directory does not have a content revision")
		}
		if err := makeParents(root, r.TargetPath); err != nil {
			return nil, err
		}
		if target, err := safeEntry(root, r.TargetPath, true, false); err != nil {
			return nil, err
		} else if target != nil {
			return nil, fileError(http.StatusConflict, "destination already exists: %s", r.TargetPath)
		}
		if err := root.Rename(r.Path, r.TargetPath); err != nil {
			return nil, err
		}
		e, err := makeEntry(root, r.TargetPath)
		return &StackFileResponse{Entry: e}, err
	case "delete":
		info, err := safeEntry(root, r.Path, false, false)
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			if err := revisionGuard(root, r.Path, r.Revision, false); err != nil {
				return nil, err
			}
		} else if r.Revision != "" {
			return nil, fileError(http.StatusConflict, "directory does not have a content revision")
		}
		return &StackFileResponse{}, root.Remove(r.Path) // directories must be empty
	case "apply":
		return applyBoundFiles(root, r.Files, r.Deletions)
	default:
		return nil, fileError(http.StatusBadRequest, "unknown stack-file action: %s", r.Action)
	}
}

func applyBoundFiles(root *os.Root, files []StackFileUpload, deletions []FileToDelete) (*StackFileResponse, error) {
	if len(files)+len(deletions) > maxStackApplyFiles {
		return nil, fileError(http.StatusForbidden, "too many files in apply request")
	}
	total := 0
	data := make([][]byte, len(files))
	seen := map[string]bool{}
	for i, f := range files {
		if err := validateFileRel(f.Path); err != nil {
			return nil, err
		}
		for prior := range seen {
			if prior == f.Path || strings.HasPrefix(f.Path, prior+"/") || strings.HasPrefix(prior, f.Path+"/") {
				return nil, fileError(http.StatusForbidden, "conflicting apply paths: %s and %s", prior, f.Path)
			}
		}
		seen[f.Path] = true
		b, err := decodeStackFile(f.ContentBase64)
		if err != nil {
			return nil, err
		}
		total += len(b)
		if total > maxStackApplyBytes {
			return nil, fileError(http.StatusForbidden, "apply exceeds 32 MiB")
		}
		data[i] = b
		// Validate the complete batch before changing a single file.
		if err := preflightUpload(root, f.Path, f.Revision); err != nil {
			return nil, err
		}
	}
	for _, d := range deletions {
		if err := validateFileRel(d.Path); err != nil {
			return nil, err
		}
		if seen[d.Path] {
			return nil, fileError(http.StatusForbidden, "file both written and deleted: %s", d.Path)
		}
		seen[d.Path] = true
	}
	for i, f := range files {
		if err := revisionGuard(root, f.Path, f.Revision, true); err != nil {
			return nil, err
		}
		if err := atomicFile(root, f.Path, data[i]); err != nil {
			return nil, err
		}
	}
	result := &StackFileResponse{DeletedFiles: []string{}, SkippedFiles: []SkippedFile{}}
	for _, d := range deletions {
		if loadBearingFiles[filepath.Base(d.Path)] {
			result.SkippedFiles = append(result.SkippedFiles, SkippedFile{Path: d.Path, Reason: "load-bearing"})
			continue
		}
		rev, _, err := fileRevision(root, d.Path)
		if err != nil {
			reason := "apply-failed"
			var e *stackFileError
			if errors.As(err, &e) && e.status == http.StatusNotFound {
				reason = "already-absent"
			} else if errors.As(err, &e) && e.status == http.StatusForbidden {
				reason = "locally-modified"
			}
			result.SkippedFiles = append(result.SkippedFiles, SkippedFile{Path: d.Path, Reason: reason})
			continue
		}
		if rev != d.Sha256 {
			result.SkippedFiles = append(result.SkippedFiles, SkippedFile{Path: d.Path, Reason: "locally-modified"})
			continue
		}
		if err = root.Remove(d.Path); err != nil {
			result.SkippedFiles = append(result.SkippedFiles, SkippedFile{Path: d.Path, Reason: "apply-failed"})
			continue
		}
		result.DeletedFiles = append(result.DeletedFiles, d.Path)
	}
	sort.Strings(result.DeletedFiles)
	return result, nil
}

// Handle accepts one bounded JSON request and returns a transport-neutral JSON
// status/body. Standard and Edge use this exact handler rather than duplicate
// authorization, revision, or path checking code.
func (s *StackFileService) Handle(ctx context.Context, body []byte) (int, []byte) {
	if len(body) > maxStackRequestBytes {
		return stackFileResponse(http.StatusRequestEntityTooLarge, nil, fileError(http.StatusRequestEntityTooLarge, "stack-file request too large"))
	}
	var r StackFileRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return stackFileResponse(http.StatusBadRequest, nil, fileError(http.StatusBadRequest, "invalid stack-file request: %v", err))
	}
	result, err := s.action(ctx, r)
	if err != nil {
		var e *stackFileError
		status := http.StatusInternalServerError
		if errors.As(err, &e) {
			status = e.status
		} else if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		return stackFileResponse(status, nil, err)
	}
	return stackFileResponse(http.StatusOK, result, nil)
}
func stackFileResponse(status int, r *StackFileResponse, err error) (int, []byte) {
	if err != nil {
		r = &StackFileResponse{Error: err.Error()}
	}
	b, _ := json.Marshal(r)
	return status, b
}
