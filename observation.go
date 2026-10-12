package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ObservationManifest is an opt-in, portable record of one sync scan. Its
// schema version is independent of the checksum database format.
type ObservationManifest struct {
	SchemaVersion int               `json:"schema_version"`
	ScanID        string            `json:"scan_id"`
	GeneratedAt   int64             `json:"generated_at"`
	Roots         []string          `json:"roots"`
	Partial       bool              `json:"partial"`
	Observations  []FileObservation `json:"observations"`
	Coverage      []RootCoverage    `json:"coverage"`
}

// FileObservation records the bytes seen by the scanner. It is evidence only;
// consumers must make their own upload, baseline, and deletion decisions.
type FileObservation struct {
	Root          string     `json:"root"`
	RootDevice    uint64     `json:"root_device,omitempty"`
	RootInode     uint64     `json:"root_inode,omitempty"`
	Path          string     `json:"path"`
	OriginalPath  string     `json:"original_path,omitempty"`
	CurrentPath   string     `json:"current_path,omitempty"`
	Category      string     `json:"category"`
	Baseline      *FileEntry `json:"baseline,omitempty"`
	Observed      *FileEntry `json:"observed,omitempty"`
	Device        uint64     `json:"device,omitempty"`
	Inode         uint64     `json:"inode,omitempty"`
	ScanID        string     `json:"scan_id"`
	ObservationID string     `json:"observation_id"`
	Error         string     `json:"error,omitempty"`
}

// RootCoverage says whether enumeration of one requested root completed.
type RootCoverage struct {
	Root      string `json:"root"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Completed int    `json:"completed_observations"`
	Device    uint64 `json:"device,omitempty"`
	Inode     uint64 `json:"inode,omitempty"`
}

func newManifest(roots []string) *ObservationManifest {
	now := time.Now()
	return &ObservationManifest{
		SchemaVersion: 1,
		ScanID:        now.UTC().Format("20060102T150405.000000000Z"),
		GeneratedAt:   now.Unix(),
		Roots:         append([]string(nil), roots...),
		Observations:  []FileObservation{},
		Coverage:      make([]RootCoverage, 0, len(roots)),
	}
}

func observation(result WorkerResult, category string, baseline *FileEntry, scanID string) FileObservation {
	var observed *FileEntry
	if result.Exists && result.Err == nil {
		observed = &FileEntry{Checksum: result.Checksum, Size: result.Size, ModTime: result.ModTime}
	}
	item := FileObservation{
		Path: result.FilePath, Category: category, Baseline: baseline,
		Observed: observed, Device: result.Device, Inode: result.Inode,
		ScanID: scanID,
	}
	if result.Err != nil {
		item.Error = result.Err.Error()
	}
	setObservationIdentity(&item)
	return item
}

func setObservationIdentity(item *FileObservation) {
	item.ObservationID = ""
	identity, _ := json.Marshal(item)
	sum := sha256.Sum256(identity)
	item.ObservationID = fmt.Sprintf("%x", sum[:])
}

func validateManifestVersion(m *ObservationManifest) error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("unsupported observation manifest schema version %d", m.SchemaVersion)
	}
	return nil
}

func absoluteManifestRoot(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return filepath.Clean(root)
	}
	resolved, err := resolveWalkRoot(abs)
	if err != nil {
		return abs
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return filepath.Clean(resolved)
	}
	return resolved
}

func getSyncFilesForManifest(walkRoots, filterDirs []string, db *ChecksumDB) ([]string, []RootCoverage, error) {
	if len(walkRoots) == 0 {
		return nil, nil, fmt.Errorf("sync mode requires at least one directory argument or config directories")
	}
	resolvedFilters := make([]string, 0, len(filterDirs))
	for _, root := range filterDirs {
		if resolved, err := resolveWalkRoot(root); err == nil {
			resolvedFilters = append(resolvedFilters, resolved)
		} else {
			resolvedFilters = append(resolvedFilters, root)
		}
	}
	dbPaths, err := dbPathsInScope(resolvedFilters, db)
	if err != nil {
		return nil, nil, err
	}
	coverage := make([]RootCoverage, 0, len(walkRoots))
	discovered := make([]string, 0)
	for _, root := range walkRoots {
		files, state := walkManifestRoot(root, db)
		coverage = append(coverage, state)
		discovered = append(discovered, files...)
	}
	files := make([]string, 0, len(dbPaths)+len(discovered))
	seen := make(map[string]bool, len(dbPaths)+len(discovered))
	covered := make(map[string]bool)
	for _, path := range dbPaths {
		for _, root := range coverage {
			if path == root.Root || isUnderDir(path, root.Root) {
				covered[path] = true
				if !seen[path] {
					files = append(files, path)
					seen[path] = true
				}
				break
			}
		}
		if !covered[path] {
			root := filepath.Dir(path)
			found := false
			for i := range coverage {
				if coverage[i].Root == root {
					found = true
					break
				}
			}
			if !found {
				coverage = append(coverage, RootCoverage{Root: root, Status: "uncovered", Error: "tracked path is outside the requested walk roots"})
			}
			if !seen[path] {
				files = append(files, path)
				seen[path] = true
			}
		}
	}
	for _, path := range discovered {
		if !seen[path] {
			files = append(files, path)
			seen[path] = true
		}
	}
	return files, coverage, nil
}

func walkManifestRoot(root string, db *ChecksumDB) ([]string, RootCoverage) {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	resolved, err := resolveWalkRoot(abs)
	if err != nil {
		status := "unreadable"
		if errors.Is(err, os.ErrNotExist) {
			status = "missing"
		}
		return nil, RootCoverage{Root: abs, Status: status, Error: err.Error()}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, RootCoverage{Root: abs, Status: "unreadable", Error: err.Error()}
	}
	state := RootCoverage{Root: resolved, Status: "complete"}
	if info, statErr := os.Stat(resolved); statErr == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			state.Device, state.Inode = uint64(stat.Dev), stat.Ino
		}
	}
	files := make([]string, 0)
	err = filepath.WalkDir(resolved, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			state.Status = "incomplete"
			state.Error = walkErr.Error()
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			targetInfo, statErr := os.Stat(path)
			if statErr == nil && targetInfo.IsDir() {
				return nil
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		absPath, absErr := filepath.Abs(path)
		if absErr != nil {
			state.Status, state.Error = "incomplete", absErr.Error()
			return absErr
		}
		if _, ok := db.Checksums[absPath]; !ok {
			files = append(files, absPath)
		}
		return nil
	})
	if err != nil && state.Error == "" {
		state.Status, state.Error = "incomplete", err.Error()
	}
	return files, state
}

func saveObservationManifest(path string, manifest *ObservationManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal observation manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(path, data); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func loadObservationManifest(path string) (*ObservationManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	manifest := &ObservationManifest{}
	if err := json.Unmarshal(data, manifest); err != nil {
		return nil, err
	}
	if err := validateManifestVersion(manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

type ObservationApplyResult struct {
	ObservationID string `json:"observation_id"`
	Path          string `json:"path"`
	Category      string `json:"category"`
	Status        string `json:"status"`
	Detail        string `json:"detail,omitempty"`
}

func applyObservationManifest(db *ChecksumDB, manifest *ObservationManifest, selected map[string]bool, acceptSuspect, withDeleted bool) ([]ObservationApplyResult, bool, error) {
	if err := validateManifestVersion(manifest); err != nil {
		return nil, false, err
	}
	if manifest.Partial {
		// Positive observations may still be selected, but absence is never
		// inferred from incomplete coverage.
	}
	byID := make(map[string]FileObservation, len(manifest.Observations))
	for _, item := range manifest.Observations {
		if item.ObservationID == "" {
			return nil, false, fmt.Errorf("manifest contains an observation without an identity")
		}
		if _, exists := byID[item.ObservationID]; exists {
			return nil, false, fmt.Errorf("manifest contains duplicate observation identity %s", item.ObservationID)
		}
		claimedID := item.ObservationID
		setObservationIdentity(&item)
		if item.ObservationID != claimedID {
			return nil, false, fmt.Errorf("observation identity does not match its facts: %s", claimedID)
		}
		item.ObservationID = claimedID
		byID[item.ObservationID] = item
	}
	selectedIDs := make([]string, 0, len(selected))
	for id := range selected {
		selectedIDs = append(selectedIDs, id)
	}
	sort.Strings(selectedIDs)
	results := make([]ObservationApplyResult, 0, len(selected))
	changed := false
	for _, id := range selectedIDs {
		item, ok := byID[id]
		if !ok {
			results = append(results, ObservationApplyResult{ObservationID: id, Status: "failed", Detail: "identity is not in this manifest"})
			continue
		}
		result := ObservationApplyResult{ObservationID: id, Path: item.Path, Category: item.Category}
		if item.Category == "error" || item.Category == "unstable" {
			result.Status, result.Detail = "skipped", "observation is incomplete"
			results = append(results, result)
			continue
		}
		if item.Category == "move_source" {
			result.Status, result.Detail = "skipped", "apply the matching moved observation atomically"
			results = append(results, result)
			continue
		}
		current, exists := db.Checksums[item.Path]
		if item.Category == "deleted" {
			if !withDeleted {
				result.Status, result.Detail = "skipped", "deletion requires -with-deleted"
			} else if !rootCoverageComplete(manifest, item.Root) {
				result.Status, result.Detail = "skipped", "root coverage is incomplete"
			} else if !rootCurrentlyMatches(manifest, item) {
				result.Status, result.Detail = "conflicted", "source root is unavailable or changed since observation"
			} else if absent, err := pathConfirmedAbsent(item.Path); err != nil {
				result.Status, result.Detail = "conflicted", "cannot confirm path absence: "+err.Error()
			} else if !absent {
				result.Status, result.Detail = "conflicted", "path exists again"
			} else if !exists {
				result.Status = "already_applied"
			} else if item.Baseline == nil || current != *item.Baseline {
				result.Status, result.Detail = "conflicted", "baseline entry changed since observation"
			} else {
				delete(db.Checksums, item.Path)
				result.Status, changed = "applied", true
			}
			results = append(results, result)
			continue
		}
		if item.Category == "moved" {
			old, oldExists := db.Checksums[item.OriginalPath]
			if !oldExists && exists && item.Observed != nil && current == *item.Observed {
				absent, err := pathConfirmedAbsent(item.OriginalPath)
				if err == nil && absent {
					matches, matchErr := observedBytesStillMatch(item)
					if matchErr == nil && matches {
						result.Status = "already_applied"
						results = append(results, result)
						continue
					}
				}
			}
			if !oldExists || item.Baseline == nil || old != *item.Baseline || exists {
				result.Status, result.Detail = "conflicted", "move source or destination baseline changed since observation"
				results = append(results, result)
				continue
			}
			if !rootCurrentlyMatches(manifest, item) {
				result.Status, result.Detail = "conflicted", "source root is unavailable or changed since observation"
				results = append(results, result)
				continue
			}
			absent, err := pathConfirmedAbsent(item.OriginalPath)
			if err != nil {
				result.Status, result.Detail = "conflicted", "cannot confirm move source absence: "+err.Error()
				results = append(results, result)
				continue
			}
			if !absent {
				result.Status, result.Detail = "conflicted", "move source path exists again"
				results = append(results, result)
				continue
			}
		}
		if item.Observed == nil {
			result.Status, result.Detail = "skipped", "observation has no completed file digest"
			results = append(results, result)
			continue
		}
		if exists && current == *item.Observed {
			matches, err := observedBytesStillMatch(item)
			if err == nil && matches {
				result.Status = "already_applied"
				results = append(results, result)
				continue
			}
		}
		if item.Category == "corrupt" || item.Category == "unverifiable" || item.Category == "modified" {
			if !acceptSuspect {
				result.Status, result.Detail = "skipped", "content acceptance requires -accept-suspect"
				results = append(results, result)
				continue
			}
		} else if item.Category != "verified" && item.Category != "new" && item.Category != "moved" {
			result.Status, result.Detail = "skipped", "unsupported observation category"
			results = append(results, result)
			continue
		}
		if item.Category == "new" {
			if exists {
				result.Status, result.Detail = "conflicted", "path was added to the baseline after observation"
				results = append(results, result)
				continue
			}
		} else if item.Category != "moved" && (!exists || item.Baseline == nil || current != *item.Baseline) {
			result.Status, result.Detail = "conflicted", "baseline entry changed since observation"
			results = append(results, result)
			continue
		}
		matches, err := observedBytesStillMatch(item)
		if err != nil || !matches {
			result.Status, result.Detail = "conflicted", "current bytes differ from the observed bytes"
			if err != nil {
				result.Detail = err.Error()
			}
			results = append(results, result)
			continue
		}
		if item.Category == "moved" {
			delete(db.Checksums, item.OriginalPath)
		}
		db.Checksums[item.Path] = *item.Observed
		db.markVerified(item.Path, time.Now().Unix())
		result.Status, changed = "applied", true
		results = append(results, result)
	}
	return results, changed, nil
}

func observedBytesStillMatch(item FileObservation) (bool, error) {
	checksum, info, err := calculateStableChecksum(item.Path)
	if err != nil {
		return false, err
	}
	if item.Device != 0 || item.Inode != 0 {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint64(stat.Dev) != item.Device || stat.Ino != item.Inode {
			return false, fmt.Errorf("source file identity changed since observation")
		}
	}
	return checksum == item.Observed.Checksum && info.Size() == item.Observed.Size && info.ModTime().UnixNano() == item.Observed.ModTime, nil
}

func pathConfirmedAbsent(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return false, err
}

func rootCoverageComplete(manifest *ObservationManifest, root string) bool {
	if root == "" {
		return false
	}
	for _, coverage := range manifest.Coverage {
		if coverage.Root == root && coverage.Status == "complete" {
			return true
		}
	}
	return false
}

func rootCurrentlyMatches(manifest *ObservationManifest, item FileObservation) bool {
	for _, coverage := range manifest.Coverage {
		if coverage.Root != item.Root || coverage.Status != "complete" {
			continue
		}
		info, err := os.Stat(item.Root)
		if err != nil || !info.IsDir() {
			return false
		}
		if coverage.Device == 0 && coverage.Inode == 0 {
			return true
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		return ok && uint64(stat.Dev) == coverage.Device && stat.Ino == coverage.Inode &&
			item.RootDevice == coverage.Device && item.RootInode == coverage.Inode
	}
	return false
}

func coverageIsComplete(manifest *ObservationManifest, root string) bool {
	return rootCoverageComplete(manifest, root)
}

func buildTargetedManifest(paths []string, db *ChecksumDB) (*ObservationManifest, int) {
	roots := make([]string, 0, len(paths))
	rootSeen := make(map[string]bool)
	absPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = filepath.Clean(path)
		}
		absPaths = append(absPaths, abs)
		root := filepath.Dir(abs)
		if !rootSeen[root] {
			roots = append(roots, root)
			rootSeen[root] = true
		}
	}
	manifest := newManifest(roots)
	manifest.Partial = true // a targeted check never establishes tree coverage
	errorCount := 0
	for _, root := range roots {
		coverage := RootCoverage{Root: root, Status: "targeted"}
		if info, err := os.Stat(root); err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				coverage.Device, coverage.Inode = uint64(stat.Dev), stat.Ino
			}
		}
		manifest.Coverage = append(manifest.Coverage, coverage)
	}
	for _, path := range absPaths {
		entry, inDB := db.Checksums[path]
		baseline := (*FileEntry)(nil)
		if inDB {
			baseline = &entry
		}
		checksum, info, err := calculateStableChecksum(path)
		if err != nil {
			category := "error"
			if errors.Is(err, errUnstableRead) {
				category = "unstable"
			} else if os.IsNotExist(err) {
				category = "unconfirmed_absence"
			}
			result := observation(WorkerResult{FilePath: path, Err: err}, category, baseline, manifest.ScanID)
			result.Root = filepath.Dir(path)
			setObservationIdentity(&result)
			manifest.Observations = append(manifest.Observations, result)
			errorCount++
			continue
		}
		result := WorkerResult{FilePath: path, Exists: true, Checksum: checksum, Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			result.Device, result.Inode = uint64(stat.Dev), stat.Ino
		}
		category := "new"
		if inDB {
			if entry.Checksum == checksum {
				category = "verified"
			} else {
				switch classifyMismatch(entry, result.Size, result.ModTime) {
				case mismatchCorrupt:
					category = "corrupt"
				case mismatchUnverifiable:
					category = "unverifiable"
				default:
					category = "modified"
				}
				errorCount++
			}
		}
		item := observation(result, category, baseline, manifest.ScanID)
		item.Root = filepath.Dir(path)
		setObservationIdentity(&item)
		manifest.Observations = append(manifest.Observations, item)
	}
	return manifest, errorCount
}

func verifyObservationManifest(manifest *ObservationManifest, selected map[string]bool, alternateRoot string) ([]ObservationApplyResult, error) {
	if err := validateManifestVersion(manifest); err != nil {
		return nil, err
	}
	byID := make(map[string]FileObservation, len(manifest.Observations))
	for _, item := range manifest.Observations {
		claimed := item.ObservationID
		setObservationIdentity(&item)
		if claimed == "" || item.ObservationID != claimed {
			return nil, fmt.Errorf("manifest contains an invalid observation identity")
		}
		item.ObservationID = claimed
		if _, exists := byID[claimed]; exists {
			return nil, fmt.Errorf("manifest contains duplicate observation identity %s", claimed)
		}
		byID[claimed] = item
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rootSeen := ""
	rootAbs, err := filepath.Abs(alternateRoot)
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, fmt.Errorf("resolve alternate root: %w", err)
	}
	results := make([]ObservationApplyResult, 0, len(ids))
	for _, id := range ids {
		item, ok := byID[id]
		result := ObservationApplyResult{ObservationID: id}
		if !ok {
			result.Status, result.Detail = "failed", "identity is not in this manifest"
			results = append(results, result)
			continue
		}
		result.Path, result.Category = item.Path, item.Category
		if item.Observed == nil || item.Root == "" || item.Category == "deleted" || item.Category == "unconfirmed_absence" || item.Category == "error" || item.Category == "unstable" {
			result.Status, result.Detail = "failed", "observation has no verifiable positive file content"
			results = append(results, result)
			continue
		}
		if rootSeen == "" {
			rootSeen = item.Root
		} else if rootSeen != item.Root {
			return nil, fmt.Errorf("selected observations span multiple roots; verify each root separately")
		}
		rel, err := filepath.Rel(item.Root, item.Path)
		if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			result.Status, result.Detail = "failed", "observation path is outside its recorded root"
			results = append(results, result)
			continue
		}
		target := filepath.Join(rootAbs, rel)
		resolvedTarget, err := filepath.EvalSymlinks(target)
		if err != nil {
			result.Status, result.Detail = "failed", err.Error()
			results = append(results, result)
			continue
		}
		if !isUnderDir(resolvedTarget, resolvedRoot) {
			result.Status, result.Detail = "failed", "alternate path resolves outside the supplied root"
			results = append(results, result)
			continue
		}
		checksum, info, err := calculateStableChecksum(resolvedTarget)
		if err != nil {
			result.Status, result.Detail = "failed", err.Error()
		} else if checksum != item.Observed.Checksum || info.Size() != item.Observed.Size {
			result.Status, result.Detail = "failed", "copied bytes do not match the observation"
		} else {
			result.Status = "verified"
		}
		results = append(results, result)
	}
	return results, nil
}
