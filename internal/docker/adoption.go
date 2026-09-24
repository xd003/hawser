package docker

// Transactional adoption of a stack directory that already exists on the
// Hawser host. The request deliberately contains a source path only for the
// prepare phase. Finalize and rollback use the durable journal, so they cannot
// be turned into arbitrary host-path deletion primitives.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
)

var safeAdoptionName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var safeAdoptionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type StackDirAdoptionRequest struct {
	AdoptionID             string           `json:"adoptionId"`
	ProjectName            string           `json:"projectName"`
	SourceDir              string           `json:"sourceDir"`
	SourceComposeFiles     []string         `json:"sourceComposeFiles"`
	SourceEnvPath          string           `json:"sourceEnvPath,omitempty"`
	PreservedEnvRelative   string           `json:"preservedEnvRelativePath,omitempty"`
	PreserveExistingEnv    bool             `json:"preserveExistingEnv"`
	ExplicitGitEnvRelative string           `json:"explicitGitEnvRelativePath,omitempty"`
	Compose                ComposeOperation `json:"compose"`
}

type StackDirAdoptionIDRequest struct {
	AdoptionID string `json:"adoptionId"`
}

type StackDirAccessRequest struct {
	SourceDir string `json:"sourceDir"`
}

type StackDirAdoptionResult struct {
	Success             bool     `json:"success"`
	AdoptionID          string   `json:"adoptionId,omitempty"`
	Phase               string   `json:"phase,omitempty"`
	SameDirectory       bool     `json:"sameDirectory,omitempty"`
	ManagedDirectory    string   `json:"managedDirectory,omitempty"`
	ManagedComposeFiles []string `json:"managedComposeFiles,omitempty"`
	ManagedEnvRelative  string   `json:"managedEnvRelativePath,omitempty"`
	ManagedEnvContent   *string  `json:"managedEnvContent,omitempty"`
	Output              string   `json:"output,omitempty"`
	Error               string   `json:"error,omitempty"`
	ExitCode            int      `json:"exitCode,omitempty"`
	RollbackError       string   `json:"rollbackError,omitempty"`
}

func CheckStackDirAccess(req *StackDirAccessRequest) *StackDirAdoptionResult {
	source, err := canonicalExisting(req.SourceDir)
	if err != nil {
		return &StackDirAdoptionResult{Success: false, Error: fmt.Sprintf("source directory is not accessible: %v", err)}
	}
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return &StackDirAdoptionResult{Success: false, Error: "source path is not a directory"}
	}
	if err := rejectSpecialTree(source); err != nil {
		return &StackDirAdoptionResult{Success: false, Error: err.Error()}
	}
	return &StackDirAdoptionResult{Success: true}
}

type adoptionJournal struct {
	AdoptionID          string           `json:"adoptionId"`
	ProjectName         string           `json:"projectName"`
	SourceDir           string           `json:"sourceDir"`
	DestinationDir      string           `json:"destinationDir"`
	SnapshotDir         string           `json:"snapshotDir,omitempty"`
	JournalDir          string           `json:"journalDir"`
	SourceIdentity      adoptionIdentity `json:"sourceIdentity"`
	DestinationIdentity adoptionIdentity `json:"destinationIdentity"`
	SameDirectory       bool             `json:"sameDirectory"`
	ManagedEnvRelative  string           `json:"managedEnvRelativePath,omitempty"`
	ManagedEnvContent   *string          `json:"managedEnvContent,omitempty"`
	ManagedComposeFiles []string         `json:"managedComposeFiles,omitempty"`
	Output              string           `json:"output,omitempty"`
	ExitCode            int              `json:"exitCode,omitempty"`
	Phase               string           `json:"phase"`
}

type adoptionIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

var adoptionMu sync.Mutex

func (c *ComposeClient) adoptionRoot() string {
	return filepath.Join(c.stacksDir, ".dockhand-adoptions")
}

func (c *ComposeClient) adoptionRoots() []string {
	root, err := filepath.Abs(c.stacksDir)
	if err != nil {
		return []string{c.adoptionRoot()}
	}
	primary := filepath.Join(root, ".dockhand-adoptions")
	secondary := filepath.Join(filepath.Dir(root), ".dockhand-adoptions")
	if primary == secondary {
		return []string{primary}
	}
	return []string{primary, secondary}
}

func (c *ComposeClient) adoptionJournalDir(source, id string) string {
	for _, root := range c.adoptionRoots() {
		if !pathInside(root, source) {
			return filepath.Join(root, id)
		}
	}
	return filepath.Join(c.adoptionRoots()[0], id)
}

func newAdoptionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "adoption-" + fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

func (c *ComposeClient) journalPath(id string) string {
	return filepath.Join(c.adoptionRoot(), id, "transaction.json")
}

func (c *ComposeClient) journalPaths(id string) []string {
	paths := make([]string, 0, len(c.adoptionRoots()))
	for _, root := range c.adoptionRoots() {
		paths = append(paths, filepath.Join(root, id, "transaction.json"))
	}
	return paths
}

func writeJournal(j adoptionJournal) error {
	if err := os.MkdirAll(j.JournalDir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(j.JournalDir, "transaction.json.tmp")
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(j.JournalDir, "transaction.json"))
}

func readJournal(path string) (adoptionJournal, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return adoptionJournal{}, err
	}
	var j adoptionJournal
	if err := json.Unmarshal(b, &j); err != nil {
		return adoptionJournal{}, err
	}
	if !safeAdoptionName.MatchString(j.ProjectName) || !safeAdoptionID(j.AdoptionID) || j.JournalDir == "" {
		return adoptionJournal{}, fmt.Errorf("invalid adoption journal")
	}
	if filepath.Clean(j.JournalDir) != filepath.Clean(filepath.Dir(path)) {
		return adoptionJournal{}, fmt.Errorf("adoption journal directory mismatch")
	}
	return j, nil
}

func safeAdoptionID(id string) bool {
	return safeAdoptionIDPattern.MatchString(id)
}

func fileIdentity(path string) (adoptionIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return adoptionIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return adoptionIdentity{}, fmt.Errorf("filesystem does not expose stable directory identity")
	}
	return adoptionIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func identityMatches(path string, expected adoptionIdentity) bool {
	actual, err := fileIdentity(path)
	return err == nil && actual == expected
}

func normalizeRelativePath(value string) (string, error) {
	value = filepath.ToSlash(value)
	if value == "" || filepath.IsAbs(value) {
		return "", fmt.Errorf("invalid relative path: %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid relative path: %q", value)
		}
	}
	return value, nil
}

func canonicalExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func pathInside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "."
}

func pathWithin(path, root string) bool {
	return path == root || pathInside(path, root)
}

func rejectSpecialTree(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := filepath.EvalSymlinks(path)
			if readErr != nil || !pathWithin(target, root) {
				return fmt.Errorf("symlink escapes the stack directory: %s", path)
			}
			return nil
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("unsupported special file in stack directory: %s", path)
		}
		return nil
	})
}

func copyTree(src, dst string) error {
	sourceRoot, err := canonicalExisting(src)
	if err != nil {
		return err
	}
	return copyTreeWithin(src, dst, sourceRoot, dst)
}

func copyTreeWithin(src, dst, sourceRoot, destinationRoot string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			resolvedTarget, resolveErr := canonicalExisting(src)
			if resolveErr != nil {
				return resolveErr
			}
			resolvedTarget, resolveErr = canonicalExisting(target)
			if resolveErr != nil || !pathWithin(resolvedTarget, sourceRoot) {
				return fmt.Errorf("symlink escapes the stack directory: %s", src)
			}
			relTarget, relErr := filepath.Rel(sourceRoot, resolvedTarget)
			if relErr != nil {
				return relErr
			}
			target = filepath.Join(destinationRoot, relTarget)
		}
		return os.Symlink(target, dst)
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyTreeWithin(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()), sourceRoot, destinationRoot); err != nil {
				return err
			}
		}
		if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(dst, info.ModTime(), info.ModTime())
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported special file: %s", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

func overlayFiles(root string, files map[string]string, preserve string, explicit string) error {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" || filepath.IsAbs(key) || strings.Contains(key, "\\") {
			return fmt.Errorf("invalid adoption file path: %q", key)
		}
		for _, part := range strings.Split(key, "/") {
			if part == "" || part == "." || part == ".." {
				return fmt.Errorf("invalid adoption file path: %q", key)
			}
		}
		if preserve != "" && key == preserve && explicit == "" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(key))
		if !pathInside(path, root) {
			return fmt.Errorf("adoption file escapes managed directory: %q", key)
		}
		data := []byte(files[key])
		if strings.HasPrefix(files[key], "base64:") {
			var err error
			data, err = base64.StdEncoding.DecodeString(files[key][7:])
			if err != nil {
				return fmt.Errorf("decode %s: %w", key, err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if existing, err := os.Lstat(path); err == nil && existing.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite symlink: %s", key)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func (c *ComposeClient) verifyAdoptionSource(ctx context.Context, req *StackDirAdoptionRequest) (string, string, error) {
	if c.dockerClient == nil {
		return "", "", fmt.Errorf("Docker ownership validation is unavailable")
	}
	if !safeAdoptionName.MatchString(req.ProjectName) {
		return "", "", fmt.Errorf("invalid Compose project name")
	}
	if !safeAdoptionID(req.AdoptionID) {
		return "", "", fmt.Errorf("invalid adoption id")
	}
	source, err := canonicalExisting(req.SourceDir)
	if err != nil {
		return "", "", fmt.Errorf("source directory is not accessible: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("source path is not a directory")
	}
	if err := rejectSpecialTree(source); err != nil {
		return "", "", err
	}
	managedRoot, managedErr := canonicalExisting(c.stacksDir)
	if managedErr != nil {
		return "", "", fmt.Errorf("managed stacks directory is unavailable: %w", managedErr)
	}
	if source == string(filepath.Separator) || source == managedRoot {
		return "", "", fmt.Errorf("protected source directory")
	}

	resp, err := c.dockerClient.Request(ctx, http.MethodGet, "/containers/json?all=true", nil, nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to inspect Compose containers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("failed to inspect Compose containers: status %d", resp.StatusCode)
	}
	var containers []struct {
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return "", "", err
	}
	found := false
	if len(req.SourceComposeFiles) == 0 {
		return "", "", fmt.Errorf("running Compose project did not report any config files")
	}
	requestedConfigs := make(map[string]bool)
	for _, configured := range req.SourceComposeFiles {
		path, pathErr := canonicalExisting(configured)
		if pathErr != nil || !pathInside(path, source) {
			return "", "", fmt.Errorf("requested Compose config file is not owned by the source directory")
		}
		requestedConfigs[path] = true
	}
	for _, container := range containers {
		labels := container.Labels
		if labels["com.docker.compose.project"] != req.ProjectName {
			continue
		}
		found = true
		workingDir, err := canonicalExisting(labels["com.docker.compose.project.working_dir"])
		if err != nil || workingDir != source {
			return "", "", fmt.Errorf("source directory is not the running Compose project directory")
		}
		config := labels["com.docker.compose.project.config_files"]
		if config == "" {
			return "", "", fmt.Errorf("running Compose project did not report config files")
		}
		labelConfigs := make(map[string]bool)
		for _, configured := range strings.Split(config, ",") {
			path, err := canonicalExisting(strings.TrimSpace(configured))
			if err != nil || !pathInside(path, source) {
				return "", "", fmt.Errorf("Compose config file is outside the source directory")
			}
			fileInfo, statErr := os.Stat(path)
			if statErr != nil || !fileInfo.Mode().IsRegular() {
				return "", "", fmt.Errorf("Compose config file is not a regular file")
			}
			labelConfigs[path] = true
		}
		if len(requestedConfigs) != len(labelConfigs) {
			return "", "", fmt.Errorf("requested Compose files do not match the running project")
		}
		for path := range requestedConfigs {
			if !labelConfigs[path] {
				return "", "", fmt.Errorf("requested Compose files do not match the running project")
			}
		}
	}
	if !found {
		return "", "", fmt.Errorf("no running or stopped Compose container owns the requested source project")
	}

	destination := filepath.Join(managedRoot, req.ProjectName)
	absDestination, err := filepath.Abs(destination)
	if err != nil || !pathInside(absDestination, managedRoot) {
		return "", "", fmt.Errorf("managed destination escapes HAWSER_STACKS_DIR")
	}
	if source != absDestination && (pathInside(source, absDestination) || pathInside(absDestination, source)) {
		return "", "", fmt.Errorf("source and managed destination overlap")
	}
	if existing, err := os.Lstat(destination); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("managed destination must not be a symlink")
		}
		if !existing.IsDir() {
			return "", "", fmt.Errorf("managed destination is not a directory")
		}
	}
	return source, destination, nil
}

func (c *ComposeClient) findAdoptionJournal(id string) (adoptionJournal, error) {
	var lastErr error
	for _, path := range c.journalPaths(id) {
		journal, err := readJournal(path)
		if err == nil {
			return journal, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return adoptionJournal{}, lastErr
}

func adoptionResult(j adoptionJournal, success bool, err string) *StackDirAdoptionResult {
	return &StackDirAdoptionResult{
		Success:             success,
		AdoptionID:          j.AdoptionID,
		Phase:               j.Phase,
		SameDirectory:       j.SameDirectory,
		ManagedDirectory:    j.DestinationDir,
		ManagedComposeFiles: j.ManagedComposeFiles,
		ManagedEnvRelative:  j.ManagedEnvRelative,
		ManagedEnvContent:   j.ManagedEnvContent,
		Output:              j.Output,
		Error:               err,
		ExitCode:            j.ExitCode,
	}
}

func (c *ComposeClient) cleanupAdoptionArtifacts(j adoptionJournal) error {
	var firstErr error
	for _, path := range []string{filepath.Join(j.JournalDir, "stage"), j.SnapshotDir} {
		if path == "" {
			continue
		}
		if err := os.RemoveAll(path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *ComposeClient) PrepareStackDirAdoption(ctx context.Context, req *StackDirAdoptionRequest, onLine func(string)) (*StackDirAdoptionResult, error) {
	adoptionMu.Lock()
	defer adoptionMu.Unlock()
	if req.AdoptionID == "" {
		req.AdoptionID = newAdoptionID()
	}
	if existing, err := c.findAdoptionJournal(req.AdoptionID); err == nil {
		switch existing.Phase {
		case "compose_succeeded", "finalized":
			return adoptionResult(existing, true, ""), nil
		case "rolled_back":
			return adoptionResult(existing, false, "adoption was already rolled back"), nil
		default:
			return adoptionResult(existing, false, "adoption is already in progress"), nil
		}
	}
	source, destination, err := c.verifyAdoptionSource(ctx, req)
	if err != nil {
		return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
	}
	sourceID, err := fileIdentity(source)
	if err != nil {
		return nil, fmt.Errorf("failed to identify source directory: %w", err)
	}
	journalDir := c.adoptionJournalDir(source, req.AdoptionID)
	j := adoptionJournal{
		AdoptionID: req.AdoptionID, ProjectName: req.ProjectName, SourceDir: source,
		DestinationDir: destination, JournalDir: journalDir, SourceIdentity: sourceID, Phase: "preparing",
	}
	same := source == destination
	j.SameDirectory = same
	if err := writeJournal(j); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		_ = os.RemoveAll(journalDir)
		return nil, err
	}
	if err := rejectSpecialTree(source); err != nil {
		_ = os.RemoveAll(journalDir)
		return nil, err
	}

	staging := filepath.Join(journalDir, "stage")
	if err := os.RemoveAll(staging); err != nil {
		return nil, err
	}
	if err := copyTree(source, staging); err != nil {
		os.RemoveAll(journalDir)
		return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: fmt.Sprintf("failed to stage source: %v", err), ExitCode: 1}, nil
	}

	preserved := req.PreservedEnvRelative
	if preserved != "" {
		preserved, err = normalizeRelativePath(preserved)
		if err != nil {
			_ = os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
		}
	}
	if !req.PreserveExistingEnv && req.SourceEnvPath == "" {
		preserved = ""
	}
	if req.PreserveExistingEnv && req.SourceEnvPath == "" && preserved != "" {
		if _, statErr := os.Stat(filepath.Join(staging, filepath.FromSlash(preserved))); os.IsNotExist(statErr) {
			preserved = ""
		}
	}
	if req.PreserveExistingEnv && preserved == "" && req.SourceEnvPath == "" {
		if _, statErr := os.Stat(filepath.Join(staging, ".env")); statErr == nil {
			preserved = ".env"
		}
	}
	sourceEnvOutside := false
	if req.SourceEnvPath != "" {
		envPath, envErr := canonicalExisting(req.SourceEnvPath)
		if envErr != nil {
			_ = os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: envErr.Error(), ExitCode: 1}, nil
		}
		if envInfo, statErr := os.Stat(envPath); statErr != nil || !envInfo.Mode().IsRegular() {
			_ = os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: "source environment path is not a regular file", ExitCode: 1}, nil
		}
		if !pathInside(envPath, source) {
			sourceEnvOutside = true
			name := filepath.Base(envPath)
			preserved = filepath.ToSlash(filepath.Join(".dockhand-env", name))
			if err := copyTree(envPath, filepath.Join(staging, filepath.FromSlash(preserved))); err != nil {
				_ = os.RemoveAll(journalDir)
				return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
			}
		} else if preserved == "" {
			preserved, _ = filepath.Rel(source, envPath)
			preserved = filepath.ToSlash(preserved)
		}
	}
	if req.ExplicitGitEnvRelative != "" {
		req.Compose.EnvFileName, err = normalizeRelativePath(req.ExplicitGitEnvRelative)
		if err != nil {
			_ = os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
		}
	}
	if req.ExplicitGitEnvRelative == "" && sourceEnvOutside {
		req.Compose.EnvFileName = preserved
	}
	if req.ExplicitGitEnvRelative == "" && preserved != "" {
		filtered := make(map[string]string, len(req.Compose.Files))
		for path, content := range req.Compose.Files {
			if filepath.ToSlash(path) != preserved {
				filtered[path] = content
			}
		}
		req.Compose.Files = filtered
	}
	if err := overlayFiles(staging, req.Compose.Files, preserved, req.ExplicitGitEnvRelative); err != nil {
		os.RemoveAll(journalDir)
		return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
	}

	managedEnv := preserved
	if req.ExplicitGitEnvRelative != "" {
		managedEnv = req.ExplicitGitEnvRelative
	}
	j.ManagedEnvRelative = managedEnv
	if same {
		j.SnapshotDir = filepath.Join(journalDir, "snapshot")
		j.Phase = "applying"
		if err := writeJournal(j); err != nil {
			_ = os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
		}
		// The snapshot is restored to source on rollback. Absolute internal
		// symlinks must target that final location, not the temporary snapshot.
		if err := copyTreeWithin(source, j.SnapshotDir, source, source); err != nil {
			_ = os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
		}
		if !identityMatches(destination, j.SourceIdentity) {
			_ = os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: "source directory changed during adoption", ExitCode: 1}, nil
		}
		if err := overlayFiles(destination, req.Compose.Files, preserved, req.ExplicitGitEnvRelative); err != nil {
			if rollbackErr := c.rollbackAdoptionLocked(j); rollbackErr != nil {
				return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: j.Phase, Error: fmt.Sprintf("%v; rollback failed: %v", err, rollbackErr), ExitCode: 1}, nil
			}
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
		}
	} else {
		j.Phase = "promoting"
		j.DestinationIdentity, err = fileIdentity(staging)
		if err != nil {
			_ = os.RemoveAll(journalDir)
			return nil, err
		}
		if err := writeJournal(j); err != nil {
			_ = os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
		}
		if _, err := os.Stat(destination); err == nil {
			os.RemoveAll(journalDir)
			return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: "managed destination already exists", ExitCode: 1}, nil
		}
		if err := os.Rename(staging, destination); err != nil {
			if mkdirErr := os.Mkdir(destination, 0700); mkdirErr != nil {
				_ = os.RemoveAll(journalDir)
				return nil, mkdirErr
			}
			j.DestinationIdentity, err = fileIdentity(destination)
			if err != nil {
				_ = os.RemoveAll(destination)
				_ = os.RemoveAll(journalDir)
				return nil, err
			}
			if err := writeJournal(j); err != nil {
				_ = os.RemoveAll(destination)
				_ = os.RemoveAll(journalDir)
				return nil, err
			}
			if copyErr := copyTree(staging, destination); copyErr != nil {
				_ = os.RemoveAll(destination)
				_ = os.RemoveAll(journalDir)
				return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: fmt.Sprintf("failed to promote staged directory: %v", err), ExitCode: 1}, nil
			}
			if removeErr := os.RemoveAll(staging); removeErr != nil {
				_ = os.RemoveAll(destination)
				_ = os.RemoveAll(journalDir)
				return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: removeErr.Error(), ExitCode: 1}, nil
			}
		}
	}
	j.DestinationIdentity, err = fileIdentity(destination)
	if err != nil {
		_ = c.rollbackAdoptionLocked(j)
		return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
	}
	j.Phase = "composing"
	if err := writeJournal(j); err != nil {
		_ = c.rollbackAdoptionLocked(j)
		return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: err.Error(), ExitCode: 1}, nil
	}
	req.Compose.ProjectName = req.ProjectName
	result, execErr := c.Execute(ctx, &req.Compose, onLine)
	if execErr != nil || result == nil || !result.Success {
		failure := "Compose adoption failed"
		code := 1
		if execErr != nil {
			failure = execErr.Error()
		} else if result != nil {
			failure = result.Error
			code = result.ExitCode
		}
		j.Phase = "compose_failed"
		_ = writeJournal(j)
		rollbackErr := c.rollbackAdoptionLocked(j)
		result := &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: failure, ExitCode: code}
		if rollbackErr != nil {
			result.Phase = j.Phase
			result.RollbackError = rollbackErr.Error()
		}
		return result, nil
	}
	j.Phase = "compose_succeeded"
	j.Output = result.Output
	j.ExitCode = result.ExitCode
	j.ManagedComposeFiles = append([]string(nil), req.Compose.ComposeFileNames...)
	if j.ManagedEnvRelative != "" {
		content, readErr := os.ReadFile(filepath.Join(destination, filepath.FromSlash(j.ManagedEnvRelative)))
		if readErr != nil {
			j.Phase = "compose_failed"
			_ = writeJournal(j)
			rollbackErr := c.rollbackAdoptionLocked(j)
			failure := &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Phase: "rolled_back", Error: fmt.Sprintf("failed to read managed environment file: %v", readErr), ExitCode: 1}
			if rollbackErr != nil {
				failure.Phase = j.Phase
				failure.RollbackError = rollbackErr.Error()
			}
			return failure, nil
		}
		value := string(content)
		j.ManagedEnvContent = &value
	}
	if err := writeJournal(j); err != nil {
		return nil, err
	}
	return adoptionResult(j, true, ""), nil
}

func (c *ComposeClient) rollbackAdoptionLocked(j adoptionJournal) error {
	if j.Phase == "rolled_back" || j.Phase == "finalized" {
		return nil
	}
	if j.DestinationIdentity != (adoptionIdentity{}) && !identityMatches(j.DestinationDir, j.DestinationIdentity) {
		return fmt.Errorf("managed destination changed; refusing rollback")
	}
	if j.SameDirectory {
		if _, err := os.Stat(j.SnapshotDir); err != nil {
			return fmt.Errorf("adoption snapshot is unavailable: %w", err)
		}
		if err := os.RemoveAll(j.DestinationDir); err != nil {
			return err
		}
		if err := os.Rename(j.SnapshotDir, j.DestinationDir); err != nil {
			return err
		}
	} else if err := os.RemoveAll(j.DestinationDir); err != nil {
		return err
	}
	j.Phase = "rolled_back"
	if err := writeJournal(j); err != nil {
		return err
	}
	return c.cleanupAdoptionArtifacts(j)
}

func (c *ComposeClient) adoptionAction(req *StackDirAdoptionIDRequest, action string) (*StackDirAdoptionResult, error) {
	adoptionMu.Lock()
	defer adoptionMu.Unlock()
	if !safeAdoptionID(req.AdoptionID) {
		return &StackDirAdoptionResult{Success: false, Error: "invalid adoption id"}, nil
	}
	j, err := c.findAdoptionJournal(req.AdoptionID)
	if err != nil {
		return &StackDirAdoptionResult{Success: false, AdoptionID: req.AdoptionID, Error: "adoption transaction not found"}, nil
	}
	switch action {
	case "status":
		return adoptionResult(j, true, ""), nil
	case "rollback":
		if j.Phase == "rolled_back" || j.Phase == "finalized" {
			return adoptionResult(j, true, ""), nil
		}
		if err := c.rollbackAdoptionLocked(j); err != nil {
			return &StackDirAdoptionResult{Success: false, AdoptionID: j.AdoptionID, Phase: j.Phase, Error: err.Error()}, nil
		}
		j.Phase = "rolled_back"
		return adoptionResult(j, true, ""), nil
	case "finalize":
		if j.Phase == "finalized" {
			return adoptionResult(j, true, ""), nil
		}
		if j.Phase != "compose_succeeded" {
			return &StackDirAdoptionResult{Success: false, AdoptionID: j.AdoptionID, Phase: j.Phase, Error: "adoption is not ready to finalize"}, nil
		}
		if j.DestinationIdentity != (adoptionIdentity{}) && !identityMatches(j.DestinationDir, j.DestinationIdentity) {
			return &StackDirAdoptionResult{Success: false, AdoptionID: j.AdoptionID, Phase: j.Phase, Error: "managed destination changed; refusing finalize"}, nil
		}
		if !j.SameDirectory {
			if current, realErr := canonicalExisting(j.SourceDir); realErr != nil || current != j.SourceDir || !identityMatches(j.SourceDir, j.SourceIdentity) {
				if os.IsNotExist(realErr) {
					j.Phase = "finalized"
					_ = writeJournal(j)
					_ = c.cleanupAdoptionArtifacts(j)
					return adoptionResult(j, true, ""), nil
				}
				return &StackDirAdoptionResult{Success: false, AdoptionID: j.AdoptionID, Phase: j.Phase, Error: "source directory ownership changed; refusing cleanup"}, nil
			}
			if err := os.RemoveAll(j.SourceDir); err != nil {
				return &StackDirAdoptionResult{Success: false, AdoptionID: j.AdoptionID, Phase: j.Phase, Error: err.Error()}, nil
			}
		}
		j.Phase = "finalized"
		if err := writeJournal(j); err != nil {
			return nil, err
		}
		if err := c.cleanupAdoptionArtifacts(j); err != nil {
			return &StackDirAdoptionResult{Success: false, AdoptionID: j.AdoptionID, Phase: j.Phase, Error: err.Error()}, nil
		}
		return adoptionResult(j, true, ""), nil
	default:
		return nil, fmt.Errorf("unknown adoption action")
	}
}

func (c *ComposeClient) StackDirAdoptionStatus(req *StackDirAdoptionIDRequest) (*StackDirAdoptionResult, error) {
	return c.adoptionAction(req, "status")
}

func (c *ComposeClient) FinalizeStackDirAdoption(req *StackDirAdoptionIDRequest) (*StackDirAdoptionResult, error) {
	return c.adoptionAction(req, "finalize")
}

func (c *ComposeClient) RollbackStackDirAdoption(req *StackDirAdoptionIDRequest) (*StackDirAdoptionResult, error) {
	return c.adoptionAction(req, "rollback")
}

// RecoverStackDirAdoptions cleans up staging artifacts left by an interrupted
// prepare operation. A completed Compose operation is deliberately retained
// for Dockhand to finalize or roll back explicitly.
func (c *ComposeClient) RecoverStackDirAdoptions() error {
	adoptionMu.Lock()
	defer adoptionMu.Unlock()

	seen := make(map[string]bool)
	for _, root := range c.adoptionRoots() {
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() || seen[entry.Name()] || !safeAdoptionID(entry.Name()) {
				continue
			}
			seen[entry.Name()] = true
			journal, err := readJournal(filepath.Join(root, entry.Name(), "transaction.json"))
			if err != nil {
				continue
			}
			switch journal.Phase {
			case "finalized", "rolled_back":
				if err := c.cleanupAdoptionArtifacts(journal); err != nil {
					return err
				}
			case "compose_succeeded":
				// Dockhand may still need to roll back after reconnecting.
				// In-place adoption needs its snapshot until finalize.
				if journal.SameDirectory {
					if err := os.RemoveAll(filepath.Join(journal.JournalDir, "stage")); err != nil {
						return err
					}
				} else {
					if err := c.cleanupAdoptionArtifacts(journal); err != nil {
						return err
					}
				}
			case "preparing":
				if err := os.RemoveAll(journal.JournalDir); err != nil {
					return err
				}
			case "applying", "composing", "compose_failed":
				if journal.SameDirectory {
					if _, err := os.Stat(journal.SnapshotDir); os.IsNotExist(err) {
						if err := os.RemoveAll(journal.JournalDir); err != nil {
							return err
						}
						continue
					}
				}
				if err := c.rollbackAdoptionLocked(journal); err != nil {
					return err
				}
			case "promoting":
				if _, err := os.Lstat(journal.DestinationDir); os.IsNotExist(err) {
					if err := os.RemoveAll(journal.JournalDir); err != nil {
						return err
					}
				} else if err != nil {
					return err
				} else if journal.DestinationIdentity == (adoptionIdentity{}) || !identityMatches(journal.DestinationDir, journal.DestinationIdentity) {
					return fmt.Errorf("adoption %s: managed destination changed; refusing recovery", journal.AdoptionID)
				} else if err := c.rollbackAdoptionLocked(journal); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
