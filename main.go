// checksumtool - A tool for calculating and comparing file checksums
// Copyright (C) 2024 Toni Melisma

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/cespare/xxhash/v2"
)

const clearLine = "\r\033[2K"

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// FileEntry is the value stored for each path in the database. Size and
// ModTime are what let the tool tell corruption apart from a legitimate edit:
// bit rot changes content without touching either one, so a checksum mismatch
// with unchanged metadata is a strong corruption signal. They are never used
// to skip hashing — that would defeat the point of the tool.
//
// A zero Size or ModTime means "unknown" (the file could not be stat'ed when
// the entry was written), which suppresses metadata-based inference for that
// entry rather than producing a false corruption report.
type FileEntry struct {
	Checksum uint64 `json:"checksum"`
	Size     int64  `json:"size,omitempty"`
	ModTime  int64  `json:"mtime,omitempty"`
}

// errLegacyDB is returned when a database still stores bare checksum numbers
// instead of entry objects. Loading deliberately refuses these rather than
// silently coping, so the one-time conversion happens explicitly.
var errLegacyDB = fmt.Errorf("database is in the legacy format (bare checksum numbers); run: checksumtool -mode migrate")

func (e *FileEntry) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed != "" && trimmed[0] != '{' {
		return errLegacyDB
	}

	type fileEntryAlias FileEntry
	var alias fileEntryAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*e = FileEntry(alias)
	return nil
}

// MetadataKnown reports whether the entry carries usable size and timestamp
// information. Entries written before a stat succeeded do not.
func (e FileEntry) MetadataKnown() bool {
	return e.Size > 0 && e.ModTime != 0
}

// MetadataMatches reports whether observed size and timestamp are identical to
// what was recorded. Only meaningful when MetadataKnown is true.
func (e FileEntry) MetadataMatches(size, modTime int64) bool {
	return e.Size == size && e.ModTime == modTime
}

type ChecksumDB struct {
	Checksums map[string]FileEntry `json:"checksums"`
	mutex     sync.Mutex
}

type WorkerResult struct {
	FilePath string
	Checksum uint64
	Size     int64
	ModTime  int64
	Exists   bool
	Err      error
}

func calculateChecksum(filePath string) (uint64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	hash := xxhash.New()
	if _, err := io.Copy(hash, file); err != nil {
		return 0, err
	}

	return hash.Sum64(), nil
}

func loadChecksumDB(dbFilePath string, verbose bool) (*ChecksumDB, error) {
	if verbose {
		fmt.Println("Loading checksum database...")
	}

	checksumDB := &ChecksumDB{Checksums: make(map[string]FileEntry)}
	dbData, err := os.ReadFile(dbFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			if verbose {
				fmt.Printf("Checksum database loaded with %d files\n", 0)
			}
			return checksumDB, nil
		}
		return nil, fmt.Errorf("failed to read database file: %w", err)
	}

	if err := json.Unmarshal(dbData, checksumDB); err != nil {
		if errors.Is(err, errLegacyDB) {
			return nil, errLegacyDB
		}
		return nil, fmt.Errorf("failed to parse database file: %w", err)
	}

	normalizedChecksums, err := normalizeChecksumPaths(checksumDB.Checksums)
	if err != nil {
		return nil, err
	}
	checksumDB.Checksums = normalizedChecksums

	if verbose {
		fmt.Printf("Checksum database loaded with %d files\n", len(checksumDB.Checksums))
	}

	return checksumDB, nil
}

func normalizeChecksumPaths(checksums map[string]FileEntry) (map[string]FileEntry, error) {
	normalized := make(map[string]FileEntry, len(checksums))

	for filePath, entry := range checksums {
		if filePath == "" {
			return nil, fmt.Errorf("database contains an empty file path")
		}

		absPath, err := filepath.Abs(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to normalize database path %q: %w", filePath, err)
		}

		if existing, ok := normalized[absPath]; ok && existing.Checksum != entry.Checksum {
			return nil, fmt.Errorf("database contains conflicting checksums for normalized path %q", absPath)
		}

		normalized[absPath] = entry
	}

	return normalized, nil
}

// migrateChecksumDB performs the one-time conversion from the legacy format
// (path -> bare checksum) to entry objects.
//
// It deliberately records no size or timestamp. Stat'ing during migration would
// pair a possibly-stale checksum with a fresh timestamp, fabricating the claim
// that the stored checksum was valid at that timestamp — the exact claim the
// corruption check later relies on. Metadata is instead filled in by the first
// run that actually re-reads the file and confirms the checksum still matches.
func migrateChecksumDB(dbFilePath string, verbose bool) error {
	dbData, err := os.ReadFile(dbFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no database found at %s", dbFilePath)
		}
		return fmt.Errorf("failed to read database file: %w", err)
	}

	var legacy struct {
		Checksums map[string]uint64 `json:"checksums"`
	}
	if err := json.Unmarshal(dbData, &legacy); err != nil {
		// A database already using entry objects fails to decode into uint64
		// values, which is how "nothing to do" is detected.
		current := &ChecksumDB{}
		if json.Unmarshal(dbData, current) == nil {
			fmt.Printf("Database at %s is already in the current format; nothing to migrate.\n", dbFilePath)
			return nil
		}
		return fmt.Errorf("failed to parse database file: %w", err)
	}

	migrated := make(map[string]FileEntry, len(legacy.Checksums))
	for filePath, checksum := range legacy.Checksums {
		absPath, err := filepath.Abs(filePath)
		if err != nil {
			return fmt.Errorf("failed to normalize database path %q: %w", filePath, err)
		}

		if existing, ok := migrated[absPath]; ok && existing.Checksum != checksum {
			return fmt.Errorf("database contains conflicting checksums for normalized path %q", absPath)
		}
		migrated[absPath] = FileEntry{Checksum: checksum}
	}

	checksumDB := &ChecksumDB{Checksums: migrated}
	if err := saveChecksumDB(dbFilePath, checksumDB, verbose); err != nil {
		return err
	}

	fmt.Printf("Migrated %d entries to the current database format.\n", len(migrated))
	fmt.Println("Size and timestamp are recorded the first time each file is re-read and its checksum confirmed; until then a mismatch is reported as unverifiable rather than as corruption.")

	return nil
}

func saveChecksumDB(dbFilePath string, checksumDB *ChecksumDB, verbose bool) error {
	if verbose {
		fmt.Println("Saving checksum database...")
	}

	checksumDB.mutex.Lock()
	dbData, err := json.MarshalIndent(checksumDB, "", "  ")
	checksumDB.mutex.Unlock()
	if err != nil {
		return fmt.Errorf("failed to marshal database: %w", err)
	}

	if err := writeFileAtomic(dbFilePath, dbData); err != nil {
		return err
	}

	if verbose {
		fmt.Println("Checksum database saved.")
	}

	return nil
}

// writeFileAtomic writes data to a temp file in the destination directory and
// renames it into place, so an interrupted write can never leave a half-written
// database or scan report behind.
func writeFileAtomic(destPath string, data []byte) error {
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(destDir, ".checksumtool-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmpFile.Chmod(0600); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to set temp file permissions: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file to database: %w", err)
	}

	return nil
}

func lockDBFile(dbFilePath string, exclusive bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(dbFilePath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	lockPath := dbFilePath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}

	how := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}

	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("failed to acquire database lock (is another instance running?): %w", err)
	}

	return f, nil
}

func unlockDBFile(f *os.File) {
	if f == nil {
		return
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

func isUnderDir(filePath, dirPath string) bool {
	filePath = filepath.Clean(filePath)
	dirPath = filepath.Clean(dirPath)

	if !strings.HasPrefix(filePath, dirPath) {
		return false
	}
	if len(filePath) == len(dirPath) {
		return true
	}
	return filePath[len(dirPath)] == filepath.Separator
}

func walkDirectoriesForMissing(directories []string, checksumDB *ChecksumDB) ([]string, error) {
	var files []string
	for _, directory := range directories {
		walkRoot, err := resolveWalkRoot(directory)
		if err != nil {
			return nil, err
		}

		err = filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() {
				return nil
			}

			// WalkDir doesn't follow symlinks into directories, so symlink
			// loops are not a risk. Process regular files and symlinked files
			// (workers follow symlinked files when opening), but skip symlinked
			// directories so workers do not try to hash a directory.
			if d.Type()&os.ModeSymlink != 0 {
				targetInfo, statErr := os.Stat(path)
				if statErr == nil && targetInfo.IsDir() {
					return nil
				}
			} else if !d.Type().IsRegular() {
				return nil
			}

			absPath, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("failed to get absolute path for %s: %v", path, err)
			}
			if _, ok := checksumDB.Checksums[absPath]; !ok {
				files = append(files, absPath)
			}

			return nil
		})

		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// resolveWalkRoot follows a symlinked scan root to the directory it points at,
// so passing a symlink walks the target instead of stopping at the link.
//
// Only the root itself is resolved, never its ancestors. filepath.EvalSymlinks
// would resolve every component, which silently moves the walk into a different
// path space than the database: on macOS /var is a symlink to /private/var, so
// resolving ancestors makes every walked file fail to match its database entry,
// which is keyed by filepath.Abs.
func resolveWalkRoot(directory string) (string, error) {
	info, err := os.Stat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return directory, nil
	}

	resolved := directory
	for i := 0; i < 32; i++ {
		linkInfo, err := os.Lstat(resolved)
		if err != nil {
			return "", err
		}
		if linkInfo.Mode()&os.ModeSymlink == 0 {
			return resolved, nil
		}

		target, err := os.Readlink(resolved)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(resolved), target)
		}
		resolved = filepath.Clean(target)
	}

	return "", fmt.Errorf("too many levels of symbolic links resolving scan root %q", directory)
}

// dbPathsInScope returns the absolute database paths covered by the given
// directories. With no directories, every entry is in scope.
func dbPathsInScope(directories []string, checksumDB *ChecksumDB) ([]string, error) {
	absDirs := make([]string, 0, len(directories))
	for _, dir := range directories {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute path for %s: %v", dir, err)
		}
		absDirs = append(absDirs, absDir)
	}

	var paths []string
	for filePath := range checksumDB.Checksums {
		absPath, err := filepath.Abs(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute path for %s: %v", filePath, err)
		}

		if len(absDirs) == 0 {
			paths = append(paths, absPath)
			continue
		}
		for _, absDir := range absDirs {
			if isUnderDir(absPath, absDir) {
				paths = append(paths, absPath)
				break
			}
		}
	}

	return paths, nil
}

// getSyncFiles returns the single job list for a sync run: every database entry
// in scope, plus every walked file that is not in the database. Move detection
// is the join of those two sets, so both must be hashed in the same pass.
//
// filterDirs and walkRoots are deliberately distinct. walkRoots may come from
// the config file, but letting them also filter the database would silently
// narrow what gets verified.
func getSyncFiles(walkRoots, filterDirs []string, checksumDB *ChecksumDB) ([]string, error) {
	if len(walkRoots) == 0 {
		return nil, fmt.Errorf("sync mode requires at least one directory argument or config directories")
	}

	filesToProcess, err := dbPathsInScope(filterDirs, checksumDB)
	if err != nil {
		return nil, err
	}

	discovered, err := walkDirectoriesForMissing(walkRoots, checksumDB)
	if err != nil {
		return nil, err
	}

	return append(filesToProcess, discovered...), nil
}

func getFilesToProcess(mode string, directories []string, checksumDB *ChecksumDB) ([]string, bool, error) {
	var filesToProcess []string
	var calculateChecksums bool

	switch mode {
	case "check", "update":
		calculateChecksums = true
		paths, err := dbPathsInScope(directories, checksumDB)
		if err != nil {
			return nil, false, err
		}
		filesToProcess = paths
	case "list-missing":
		calculateChecksums = false
		if len(directories) == 0 {
			return nil, false, fmt.Errorf("list-missing mode requires at least one directory argument")
		}
		files, err := walkDirectoriesForMissing(directories, checksumDB)
		if err != nil {
			return nil, false, err
		}
		filesToProcess = files
	case "add-missing":
		calculateChecksums = true
		if len(directories) == 0 {
			return nil, false, fmt.Errorf("add-missing mode requires at least one directory argument")
		}
		files, err := walkDirectoriesForMissing(directories, checksumDB)
		if err != nil {
			return nil, false, err
		}
		filesToProcess = files
	case "remove-deleted", "list-deleted":
		calculateChecksums = false
		for filePath := range checksumDB.Checksums {
			if len(directories) > 0 {
				for _, dir := range directories {
					absDir, err := filepath.Abs(dir)
					if err != nil {
						return nil, false, fmt.Errorf("failed to get absolute path for %s: %v", dir, err)
					}
					if isUnderDir(filePath, absDir) {
						filesToProcess = append(filesToProcess, filePath)
						break
					}
				}
			} else {
				filesToProcess = append(filesToProcess, filePath)
			}
		}
	default:
		return nil, false, fmt.Errorf("invalid operation mode: %s", mode)
	}

	return filesToProcess, calculateChecksums, nil
}

// mismatchKind describes what a checksum mismatch most likely means, based on
// whether anything appears to have written to the file since it was recorded.
type mismatchKind int

const (
	// mismatchCorrupt: content changed while size and timestamp did not. No
	// normal write does that, so the bytes changed underneath the filesystem.
	mismatchCorrupt mismatchKind = iota
	// mismatchUnverifiable: no recorded metadata, so an edit cannot be told
	// apart from corruption. Treated as a suspect, never applied silently.
	mismatchUnverifiable
	// mismatchModified: content, size and/or timestamp all changed, which is
	// what an ordinary edit looks like.
	mismatchModified
)

func (k mismatchKind) String() string {
	switch k {
	case mismatchCorrupt:
		return "likely corruption: content changed but size and timestamp did not"
	case mismatchUnverifiable:
		return "unverifiable: no recorded size or timestamp, cannot tell an edit from corruption"
	default:
		return "modified: content and metadata both changed"
	}
}

func classifyMismatch(entry FileEntry, size, modTime int64) mismatchKind {
	if !entry.MetadataKnown() {
		return mismatchUnverifiable
	}
	if entry.MetadataMatches(size, modTime) {
		return mismatchCorrupt
	}
	return mismatchModified
}

func worker(jobs <-chan string, results chan<- WorkerResult, calculateChecksums bool, wg *sync.WaitGroup) {
	defer wg.Done()

	for filePath := range jobs {
		result := WorkerResult{FilePath: filePath, Exists: true}

		// Stat before hashing so size and timestamp describe the file as it
		// was when its content was read.
		info, err := os.Stat(filePath)
		if err != nil {
			result.Exists = false
			results <- result
			continue
		}
		result.Size = info.Size()
		result.ModTime = info.ModTime().UnixNano()

		if calculateChecksums {
			checksum, err := calculateChecksum(filePath)
			if err != nil {
				result.Err = err
				results <- result
				continue
			}
			result.Checksum = checksum
		}

		results <- result
	}
}

// syncState accumulates a sync run's classifications. Moves can only be
// resolved once every result is in — a vanished database entry cannot be
// distinguished from a moved one until the walk has finished — so sync is the
// one mode that defers its decisions instead of acting per result.
type syncState struct {
	verified     int
	touched      []ScanFile
	modified     []ScanFile
	corrupt      []ScanFile
	unverifiable []ScanFile
	disappeared  []string
	discovered   []ScanFile
}

// procOpts carries mode-specific behaviour that only some modes need.
type procOpts struct {
	sync        *syncState
	withCorrupt bool
}

func observedFile(result WorkerResult) ScanFile {
	return ScanFile{
		Path:     result.FilePath,
		Checksum: result.Checksum,
		Size:     result.Size,
		ModTime:  result.ModTime,
	}
}

// processResults consumes worker results and applies mode-specific logic.
// Reads of checksumDB.Checksums without the mutex are safe here because
// processResults is the only goroutine that modifies the map after workers
// have started, and it runs sequentially per result.
func processResults(results <-chan WorkerResult, done chan<- struct{}, mode string, checksumDB *ChecksumDB, processedFiles *uint64, outputMu *sync.Mutex, prefix string, opts procOpts) int {
	errorCount := 0

	for result := range results {
		atomic.AddUint64(processedFiles, 1)

		if result.Err != nil {
			outputMu.Lock()
			fmt.Printf("%sError processing file %s: %v\n", prefix, result.FilePath, result.Err)
			outputMu.Unlock()
			errorCount++
			continue
		}

		switch mode {
		case "check":
			if !result.Exists {
				outputMu.Lock()
				fmt.Printf("%sFile missing: %s\n", prefix, result.FilePath)
				outputMu.Unlock()
				errorCount++
			} else if entry, ok := checksumDB.Checksums[result.FilePath]; ok && result.Checksum != entry.Checksum {
				outputMu.Lock()
				fmt.Printf("%sMismatch for file: %s (%s)\n", prefix, result.FilePath, classifyMismatch(entry, result.Size, result.ModTime))
				outputMu.Unlock()
				errorCount++
			}
		case "update":
			if !result.Exists {
				outputMu.Lock()
				fmt.Printf("%sFile missing, skipping: %s\n", prefix, result.FilePath)
				outputMu.Unlock()
				continue
			}

			entry, ok := checksumDB.Checksums[result.FilePath]
			if ok && entry.Checksum == result.Checksum {
				// Content is intact. Refresh drifted or missing metadata so a
				// later corruption check compares against a current timestamp.
				if !entry.MetadataKnown() || !entry.MetadataMatches(result.Size, result.ModTime) {
					checksumDB.mutex.Lock()
					checksumDB.Checksums[result.FilePath] = FileEntry{Checksum: entry.Checksum, Size: result.Size, ModTime: result.ModTime}
					checksumDB.mutex.Unlock()
				}
				continue
			}

			if ok {
				// Overwriting the stored checksum of a file that may have
				// rotted destroys the only evidence that it rotted.
				kind := classifyMismatch(entry, result.Size, result.ModTime)
				if kind != mismatchModified && !opts.withCorrupt {
					outputMu.Lock()
					fmt.Printf("%sRefusing to overwrite %s (%s); pass -with-corrupt to accept it\n", prefix, result.FilePath, kind)
					outputMu.Unlock()
					errorCount++
					continue
				}
			}

			outputMu.Lock()
			fmt.Printf("%sChanged or new file: %s\n", prefix, result.FilePath)
			outputMu.Unlock()
			checksumDB.mutex.Lock()
			checksumDB.Checksums[result.FilePath] = FileEntry{Checksum: result.Checksum, Size: result.Size, ModTime: result.ModTime}
			checksumDB.mutex.Unlock()
		case "sync":
			state := opts.sync
			entry, inDB := checksumDB.Checksums[result.FilePath]

			if !result.Exists {
				// Only database entries matter here; a walked file that
				// vanished mid-run was never recorded in the first place.
				if inDB {
					state.disappeared = append(state.disappeared, result.FilePath)
				}
				continue
			}

			if !inDB {
				state.discovered = append(state.discovered, observedFile(result))
				continue
			}

			if entry.Checksum == result.Checksum {
				state.verified++
				if !entry.MetadataKnown() || !entry.MetadataMatches(result.Size, result.ModTime) {
					state.touched = append(state.touched, observedFile(result))
				}
				continue
			}

			kind := classifyMismatch(entry, result.Size, result.ModTime)
			outputMu.Lock()
			fmt.Printf("%sMismatch for file: %s (%s)\n", prefix, result.FilePath, kind)
			outputMu.Unlock()
			switch kind {
			case mismatchCorrupt:
				state.corrupt = append(state.corrupt, observedFile(result))
				errorCount++
			case mismatchUnverifiable:
				state.unverifiable = append(state.unverifiable, observedFile(result))
				errorCount++
			default:
				state.modified = append(state.modified, observedFile(result))
			}
		case "list-missing":
			outputMu.Lock()
			fmt.Printf("%sFile not in database: %s\n", prefix, result.FilePath)
			outputMu.Unlock()
		case "add-missing":
			if !result.Exists {
				outputMu.Lock()
				fmt.Printf("%sFile missing: %s\n", prefix, result.FilePath)
				outputMu.Unlock()
			} else if _, ok := checksumDB.Checksums[result.FilePath]; !ok {
				checksumDB.mutex.Lock()
				checksumDB.Checksums[result.FilePath] = FileEntry{Checksum: result.Checksum, Size: result.Size, ModTime: result.ModTime}
				checksumDB.mutex.Unlock()
			}
		case "remove-deleted":
			if !result.Exists {
				outputMu.Lock()
				fmt.Printf("%sFile deleted, removing from database: %s\n", prefix, result.FilePath)
				outputMu.Unlock()
				checksumDB.mutex.Lock()
				delete(checksumDB.Checksums, result.FilePath)
				checksumDB.mutex.Unlock()
			}
		case "list-deleted":
			if !result.Exists {
				outputMu.Lock()
				fmt.Printf("%sFile deleted: %s\n", prefix, result.FilePath)
				outputMu.Unlock()
			}
		}
	}
	close(done)
	return errorCount
}

func fileExists(filePath string) bool {
	_, err := os.Stat(filePath)
	return err == nil
}

// resolveMoves joins vanished database entries against files discovered on
// disk. A move is not a distinct filesystem event: it is a deletion plus an
// addition that share content, so it can only be recognised by matching the two
// sets once both are complete.
//
// Matching is by checksum, and a group is accepted only when both sides have
// equal counts. Duplicate content is normal in a media archive, and when the
// counts are equal every possible pairing yields the same database — only the
// printed pairing is arbitrary. Unequal counts are genuinely undecidable, so
// those groups fall back to being reported as deletions and additions, which
// loses nothing: the existing add and remove paths already handle them.
//
// A move can never hide corruption, because matching requires the content hash
// to be intact. A file that was both moved and corrupted simply fails to match
// and is reported as a deletion plus a new file.
func resolveMoves(disappeared []string, discovered []ScanFile, checksumDB *ChecksumDB, strict bool) (moves []ScanMove, deleted []string, added []ScanFile, ambiguous int) {
	fromGroups := make(map[uint64][]string)
	for _, path := range disappeared {
		entry, ok := checksumDB.Checksums[path]
		if !ok {
			continue
		}
		fromGroups[entry.Checksum] = append(fromGroups[entry.Checksum], path)
	}

	toGroups := make(map[uint64][]ScanFile)
	for _, file := range discovered {
		toGroups[file.Checksum] = append(toGroups[file.Checksum], file)
	}

	checksums := make([]uint64, 0, len(fromGroups))
	for checksum := range fromGroups {
		checksums = append(checksums, checksum)
	}
	sort.Slice(checksums, func(i, j int) bool { return checksums[i] < checksums[j] })

	matchedFrom := make(map[string]bool)
	matchedTo := make(map[string]bool)

	for _, checksum := range checksums {
		from := append([]string(nil), fromGroups[checksum]...)
		to := append([]ScanFile(nil), toGroups[checksum]...)
		if len(to) == 0 {
			continue
		}

		sort.Strings(from)
		sort.Slice(to, func(i, j int) bool { return to[i].Path < to[j].Path })

		if !moveGroupAcceptable(from, to, checksumDB, strict) {
			ambiguous += len(from) + len(to)
			continue
		}

		for i := range from {
			moves = append(moves, ScanMove{
				From:     from[i],
				To:       to[i].Path,
				Checksum: checksum,
				Size:     to[i].Size,
				ModTime:  to[i].ModTime,
			})
			matchedFrom[from[i]] = true
			matchedTo[to[i].Path] = true
		}
	}

	for _, path := range disappeared {
		if !matchedFrom[path] {
			deleted = append(deleted, path)
		}
	}
	for _, file := range discovered {
		if !matchedTo[file.Path] {
			added = append(added, file)
		}
	}

	sort.Strings(deleted)
	sort.Slice(added, func(i, j int) bool { return added[i].Path < added[j].Path })

	return moves, deleted, added, ambiguous
}

func moveGroupAcceptable(from []string, to []ScanFile, checksumDB *ChecksumDB, strict bool) bool {
	if strict {
		return len(from) == 1 && len(to) == 1
	}
	if len(from) != len(to) {
		return false
	}

	// Recorded sizes corroborate the checksum where they exist. Entries carried
	// over from the legacy format have no size yet, in which case there is
	// nothing to compare and the checksum stands on its own.
	fromSizes := make([]int64, 0, len(from))
	for _, path := range from {
		entry := checksumDB.Checksums[path]
		if entry.Size <= 0 {
			return true
		}
		fromSizes = append(fromSizes, entry.Size)
	}

	toSizes := make([]int64, 0, len(to))
	for _, file := range to {
		toSizes = append(toSizes, file.Size)
	}

	sort.Slice(fromSizes, func(i, j int) bool { return fromSizes[i] < fromSizes[j] })
	sort.Slice(toSizes, func(i, j int) bool { return toSizes[i] < toSizes[j] })
	for i := range fromSizes {
		if fromSizes[i] != toSizes[i] {
			return false
		}
	}

	return true
}

// ScanFile is one file as observed during a scan.
type ScanFile struct {
	Path     string `json:"path"`
	Checksum uint64 `json:"checksum"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"mtime"`
}

// ScanMove is a database entry whose content was found at a new path.
type ScanMove struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Checksum uint64 `json:"checksum"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"mtime"`
}

// ScanReport is what a sync run leaves behind so its findings can be applied
// later without re-reading every file. It lists only changes; recording the
// files that verified cleanly would make the report as large as the database.
type ScanReport struct {
	GeneratedAt  int64      `json:"generated_at"`
	DBPath       string     `json:"db_path"`
	DBSize       int64      `json:"db_size"`
	DBModTime    int64      `json:"db_mtime"`
	Partial      bool       `json:"partial"`
	Roots        []string   `json:"roots,omitempty"`
	Verified     int        `json:"verified"`
	Ambiguous    int        `json:"ambiguous,omitempty"`
	Touched      []ScanFile `json:"touched,omitempty"`
	Modified     []ScanFile `json:"modified,omitempty"`
	Corrupt      []ScanFile `json:"corrupt,omitempty"`
	Unverifiable []ScanFile `json:"unverifiable,omitempty"`
	Moved        []ScanMove `json:"moved,omitempty"`
	New          []ScanFile `json:"new,omitempty"`
	Deleted      []string   `json:"deleted,omitempty"`
}

// Empty reports whether there is nothing for apply to do.
func (r *ScanReport) Empty() bool {
	return len(r.Touched) == 0 && len(r.Modified) == 0 && len(r.Corrupt) == 0 &&
		len(r.Unverifiable) == 0 && len(r.Moved) == 0 && len(r.New) == 0 && len(r.Deleted) == 0
}

// Concerning reports how many findings warrant a human decision. Moves and
// additions are the ordinary result of using a filesystem; content that changed
// underneath a recorded checksum, or a file that is simply gone, is not.
func (r *ScanReport) Concerning() int {
	return len(r.Modified) + len(r.Corrupt) + len(r.Unverifiable) + len(r.Deleted)
}

func defaultScanReportPath(dbFilePath string) string {
	dir := filepath.Dir(dbFilePath)
	base := filepath.Base(dbFilePath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "checksums"
	}
	return filepath.Join(dir, base+".scan.json")
}

// dbFingerprint identifies the database a scan was computed against. A missing
// database yields zeroes, which will not match a recorded fingerprint.
func dbFingerprint(dbFilePath string) (int64, int64) {
	info, err := os.Stat(dbFilePath)
	if err != nil {
		return 0, 0
	}
	return info.Size(), info.ModTime().UnixNano()
}

func saveScanReport(reportPath string, report *ScanReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal scan report: %w", err)
	}
	if err := writeFileAtomic(reportPath, data); err != nil {
		return fmt.Errorf("failed to write scan report: %w", err)
	}
	return nil
}

func loadScanReport(reportPath string) (*ScanReport, error) {
	data, err := os.ReadFile(reportPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no scan report at %s; run -mode sync first", reportPath)
		}
		return nil, fmt.Errorf("failed to read scan report: %w", err)
	}

	report := &ScanReport{}
	if err := json.Unmarshal(data, report); err != nil {
		return nil, fmt.Errorf("failed to parse scan report: %w", err)
	}
	return report, nil
}

// verifyScanReport refuses reports whose findings no longer describe the
// database in front of us. The classifications were computed against one
// specific database, and an interrupted scan sees files as deleted that it
// simply never reached.
func verifyScanReport(report *ScanReport, dbFilePath string, force bool) error {
	if force {
		return nil
	}

	absDB, err := filepath.Abs(dbFilePath)
	if err != nil {
		return err
	}
	if report.DBPath != "" && report.DBPath != absDB {
		return fmt.Errorf("scan report was generated for database %s, not %s", report.DBPath, absDB)
	}
	if report.Partial {
		return fmt.Errorf("scan report is from an interrupted scan and may be incomplete; re-run -mode sync (or pass -force)")
	}

	size, modTime := dbFingerprint(dbFilePath)
	if size != report.DBSize || modTime != report.DBModTime {
		return fmt.Errorf("database changed since the scan; re-run -mode sync (or pass -force)")
	}

	return nil
}

type applyOpts struct {
	withDeleted bool
	withCorrupt bool
}

type applyStats struct {
	touched  int
	modified int
	moved    int
	added    int
	removed  int
	accepted int
	stale    int
}

// currentMetadataMatches reports whether a file still looks exactly as it did
// during the scan. That is what makes it safe to write the scanned checksum
// without reading the file again — the cheap stat stands in for the expensive
// re-hash. Anything that changed underneath is skipped rather than guessed at.
func currentMetadataMatches(path string, size, modTime int64) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() == size && info.ModTime().UnixNano() == modTime
}

func applyScanReport(checksumDB *ChecksumDB, report *ScanReport, opts applyOpts) applyStats {
	stats := applyStats{}

	setEntry := func(file ScanFile) {
		checksumDB.Checksums[file.Path] = FileEntry{Checksum: file.Checksum, Size: file.Size, ModTime: file.ModTime}
	}

	stale := func(path string) {
		stats.stale++
		fmt.Printf("Changed since the scan, skipping: %s\n", path)
	}

	// Content verified; only the recorded metadata was missing or stale.
	for _, file := range report.Touched {
		entry, ok := checksumDB.Checksums[file.Path]
		if !ok || entry.Checksum != file.Checksum {
			stale(file.Path)
			continue
		}
		if !currentMetadataMatches(file.Path, file.Size, file.ModTime) {
			stale(file.Path)
			continue
		}
		setEntry(file)
		stats.touched++
	}

	for _, file := range report.Modified {
		if !currentMetadataMatches(file.Path, file.Size, file.ModTime) {
			stale(file.Path)
			continue
		}
		setEntry(file)
		stats.modified++
	}

	for _, move := range report.Moved {
		if _, ok := checksumDB.Checksums[move.From]; !ok {
			continue
		}
		// If the source is back, this was never a move.
		if fileExists(move.From) {
			stale(move.From)
			continue
		}
		if !currentMetadataMatches(move.To, move.Size, move.ModTime) {
			stale(move.To)
			continue
		}
		delete(checksumDB.Checksums, move.From)
		setEntry(ScanFile{Path: move.To, Checksum: move.Checksum, Size: move.Size, ModTime: move.ModTime})
		stats.moved++
	}

	for _, file := range report.New {
		if _, ok := checksumDB.Checksums[file.Path]; ok {
			continue
		}
		if !currentMetadataMatches(file.Path, file.Size, file.ModTime) {
			stale(file.Path)
			continue
		}
		setEntry(file)
		stats.added++
	}

	if opts.withCorrupt {
		for _, file := range append(append([]ScanFile(nil), report.Corrupt...), report.Unverifiable...) {
			if !currentMetadataMatches(file.Path, file.Size, file.ModTime) {
				stale(file.Path)
				continue
			}
			setEntry(file)
			stats.accepted++
		}
	}

	if opts.withDeleted {
		for _, path := range report.Deleted {
			// An unmounted volume makes every file under it look deleted, so a
			// path that is readable again is never removed.
			if fileExists(path) {
				stale(path)
				continue
			}
			delete(checksumDB.Checksums, path)
			stats.removed++
		}
	}

	return stats
}

func updateProgressBar(done <-chan struct{}, totalFiles int, processedFiles *uint64, startTime time.Time, outputMu *sync.Mutex) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			processed := atomic.LoadUint64(processedFiles)
			elapsed := time.Since(startTime)
			estimatedTotal := time.Duration(0)
			if processed > 0 {
				estimatedTotal = time.Duration(int64(elapsed) * int64(totalFiles) / int64(processed))
			}
			elapsedHuman := formatDuration(elapsed)
			estimatedTotalHuman := formatDuration(estimatedTotal)
			outputMu.Lock()
			fmt.Printf("%sProcessed %d/%d files (Elapsed: %s, Estimated Total: %s)", clearLine, processed, totalFiles, elapsedHuman, estimatedTotalHuman)
			outputMu.Unlock()
		case <-done:
			outputMu.Lock()
			fmt.Println()
			outputMu.Unlock()
			return
		}
	}
}

type Config struct {
	Directories []string `toml:"directories"`
	Workers     int      `toml:"workers"`
	Verbose     bool     `toml:"verbose"`
	StrictMoves bool     `toml:"strict_moves"`
}

// resolveDirectories returns the directories to walk. Config directories only
// seed modes that need a walk root; the database-driven modes scan the whole
// database unless directories are given explicitly, so that a config file can
// never silently narrow what gets verified.
func resolveDirectories(mode string, cliDirectories, configDirectories []string) []string {
	if len(cliDirectories) > 0 {
		return cliDirectories
	}

	switch mode {
	case "list-missing", "add-missing", "sync":
		return configDirectories
	default:
		return nil
	}
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "checksumtool", "config.toml")
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if path == "" {
		return cfg, nil
	}
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	return cfg, nil
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "checksums.json"
	}
	return filepath.Join(home, ".local", "share", "checksumtool", "checksums.json")
}

const usageText = `checksumtool detects bit rot by hashing files (xxhash64) and comparing them
against a stored database of path -> {checksum, size, timestamp}.

Usage: checksumtool -mode <mode> [flags] [directories...]

Modes:
  sync            One pass over everything: verifies database entries, walks the
                  directories for files not yet recorded, and classifies each
                  file as verified, modified, corrupt, moved, new or deleted.
                  Lists only the findings that need a decision -- changed,
                  corrupt, unverifiable and deleted files; moves and additions
                  are summarized as counts unless -list-all is given. Exits 1 if
                  any of those findings are present, or on a read error.
                  Reports only, and writes a scan report; add -apply to write the
                  results to the database in the same run. Requires directories
                  (from arguments or the config file).
  apply           Apply a scan report written by a previous sync run, without
                  re-reading any file contents. Applies moved, new, modified and
                  metadata-only changes; deletions and corruption suspects need
                  -with-deleted / -with-corrupt.
  check           Verify database entries against the files on disk. Read-only.
                  Exits 1 on any mismatch, error or missing file. Directories are
                  optional and filter which entries are checked.
  update          Re-hash database entries and record what changed. Refuses to
                  overwrite a suspected-corrupt entry unless -with-corrupt.
  list-missing    List files under the given directories not in the database.
  add-missing     Add files under the given directories to the database.
  list-deleted    List database entries whose file no longer exists.
  remove-deleted  Remove database entries whose file no longer exists.
  migrate         One-time conversion of a legacy database (bare checksum
                  numbers) to the current entry format. Size and timestamp are
                  left empty and get filled in by the first run that re-reads
                  each file and confirms its checksum.

How a mismatch is classified:
  corrupt         Content changed but size and timestamp did not. No ordinary
                  write does that, so the bytes changed underneath the
                  filesystem. Never applied without -with-corrupt.
  unverifiable    Content changed and there is no recorded size or timestamp to
                  compare against, so an edit cannot be told apart from
                  corruption. Also needs -with-corrupt.
  modified        Content and metadata both changed: an ordinary edit.

Moves are detected by matching vanished database entries against newly
discovered files with the same checksum, and are only applied when both sides of
a checksum group have equal counts (-strict-moves narrows this to one-to-one).
A move can never mask corruption, because matching requires the hash to match.

Exit codes: 0 on success, 1 on mismatches, errors, or missing files. A sync
run also exits 1 on a changed or deleted file; moves and additions never fail.

Flags:
`

func main() {
	var dbFilePath string
	var verbose bool
	var listAll bool
	var mode string
	var numWorkers int
	var configPath string
	var scanReportPath string
	var applyNow bool
	var withDeleted bool
	var withCorrupt bool
	var strictMoves bool
	var force bool
	flag.StringVar(&dbFilePath, "db", defaultDBPath(), "Checksum database file location")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose output")
	flag.StringVar(&mode, "mode", "", "Operation mode: sync, apply, check, update, list-missing, add-missing, remove-deleted, list-deleted, migrate")
	flag.IntVar(&numWorkers, "workers", 4, "Number of worker goroutines")
	flag.StringVar(&configPath, "config", defaultConfigPath(), "Config file location")
	flag.StringVar(&scanReportPath, "scan-report", "", "Scan report location (default: alongside the database, as <db>.scan.json)")
	flag.BoolVar(&applyNow, "apply", false, "sync: write the scan results to the database in the same run")
	flag.BoolVar(&withDeleted, "with-deleted", false, "sync -apply / apply: also remove entries whose file is gone (skipped by default: an unmounted volume looks deleted)")
	flag.BoolVar(&withCorrupt, "with-corrupt", false, "update, sync -apply / apply: also accept content changes that could not be told apart from corruption")
	flag.BoolVar(&listAll, "list-all", false, "sync: also list moved and new files, not just the findings that need attention")
	flag.BoolVar(&strictMoves, "strict-moves", false, "Only treat a checksum group as a move when exactly one file vanished and one appeared")
	flag.BoolVar(&force, "force", false, "apply: use the scan report even if it is partial or the database changed since the scan")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usageText)
		flag.PrintDefaults()
	}
	flag.Parse()

	// Load config file
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// Track which flags were explicitly set on the command line
	explicitFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	// Apply config values where CLI flags were not explicitly set
	if !explicitFlags["workers"] && cfg.Workers > 0 {
		numWorkers = cfg.Workers
	}
	if !explicitFlags["verbose"] {
		verbose = cfg.Verbose
	}

	// Informational output exists for a human watching a run. Under cron it
	// only produces mail, and mail that arrives every month regardless of the
	// result is mail that stops being read. So when stdout is not a terminal,
	// only findings are worth printing. This overrides -verbose deliberately:
	// the same config file serves both the interactive and the unattended use,
	// and the unattended one is the one that must stay quiet.
	interactive := isTerminal()
	if !interactive {
		verbose = false
	}
	if !explicitFlags["strict-moves"] {
		strictMoves = cfg.StrictMoves
	}

	if numWorkers <= 0 {
		fmt.Println("Error: -workers must be greater than 0")
		os.Exit(1)
	}

	if mode == "" {
		fmt.Println("Please specify an operation mode using the -mode flag: sync, apply, check, update, list-missing, add-missing, remove-deleted, list-deleted, migrate")
		fmt.Println("Run 'checksumtool -help' for a description of each mode.")
		os.Exit(1)
	}

	if scanReportPath == "" {
		scanReportPath = defaultScanReportPath(dbFilePath)
	}

	walkRoots := resolveDirectories(mode, flag.Args(), cfg.Directories)

	if (mode == "list-missing" || mode == "add-missing" || mode == "sync") && len(walkRoots) == 0 {
		fmt.Printf("Error: %s mode requires at least one directory argument or config directories\n", mode)
		os.Exit(1)
	}

	// Acquire DB lock: exclusive for mutating modes, shared for read-only
	mutating := mode == "update" || mode == "add-missing" || mode == "remove-deleted" ||
		mode == "apply" || mode == "migrate" || (mode == "sync" && applyNow)
	lockFile, err := lockDBFile(dbFilePath, mutating)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	defer unlockDBFile(lockFile)

	if mode == "migrate" {
		if err := migrateChecksumDB(dbFilePath, verbose); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	checksumDB, err := loadChecksumDB(dbFilePath, verbose)
	if err != nil {
		fmt.Printf("Error loading database: %v\n", err)
		os.Exit(1)
	}

	if mode == "apply" {
		if err := runApply(dbFilePath, scanReportPath, checksumDB, applyOpts{withDeleted: withDeleted, withCorrupt: withCorrupt}, force, verbose); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if verbose {
		fmt.Println("Comparing checksums with the database...")
	}

	var filesToProcess []string
	var calculateChecksums bool
	if mode == "sync" {
		calculateChecksums = true
		filesToProcess, err = getSyncFiles(walkRoots, flag.Args(), checksumDB)
	} else {
		filesToProcess, calculateChecksums, err = getFilesToProcess(mode, walkRoots, checksumDB)
	}
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}

	totalFiles := len(filesToProcess)
	var processedFiles uint64
	startTime := time.Now()

	jobs := make(chan string, numWorkers)
	results := make(chan WorkerResult, numWorkers)
	var wg sync.WaitGroup
	var outputMu sync.Mutex

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker(jobs, results, calculateChecksums, &wg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigChan:
			fmt.Println("\nInterrupt signal received. Finishing current work...")
			cancel()
		case <-ctx.Done():
		}
	}()

	go func() {
		defer close(jobs)
		for _, filePath := range filesToProcess {
			select {
			case jobs <- filePath:
			case <-ctx.Done():
				return
			}
		}
	}()

	prefix := ""
	showProgress := false
	if verbose && isTerminal() {
		prefix = clearLine
		showProgress = true
	}

	done := make(chan struct{})
	var mismatchCount int
	state := &syncState{}
	go func() {
		mismatchCount = processResults(results, done, mode, checksumDB, &processedFiles, &outputMu, prefix, procOpts{sync: state, withCorrupt: withCorrupt})
	}()

	if showProgress {
		go updateProgressBar(done, totalFiles, &processedFiles, startTime, &outputMu)
	}

	wg.Wait()
	close(results)

	<-done

	if verbose {
		fmt.Printf("\nFinished operation in '%s' mode.\n", mode)
	}

	if mode == "sync" {
		report := buildScanReport(state, checksumDB, dbFilePath, walkRoots, strictMoves, ctx.Err() != nil)
		// A changed or deleted file is worth a non-zero exit even though it is
		// not a read error, so an unattended run reports on its own.
		concerning := mismatchCount > 0 || report.Concerning() > 0
		// An interrupted scan verified only part of the archive, so it is
		// reported even when what it did read came back clean.
		silent := !interactive && !listAll && !report.Partial && !concerning
		printScanSummary(report, listAll, silent)

		if err := saveScanReport(scanReportPath, report); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

		if !applyNow {
			if !report.Empty() && !silent {
				fmt.Printf("\nScan report written to %s\nReview it, then run: checksumtool -mode apply\n", scanReportPath)
			}
			if concerning {
				os.Exit(1)
			}
			return
		}

		stats := applyScanReport(checksumDB, report, applyOpts{withDeleted: withDeleted, withCorrupt: withCorrupt})
		if !silent {
			printApplyStats(stats)
		}
		if err := saveChecksumDB(dbFilePath, checksumDB, verbose); err != nil {
			fmt.Printf("Error saving database: %v\n", err)
			os.Exit(1)
		}
		// The report describes a database that no longer exists; leaving it
		// behind would invite applying it a second time.
		if err := os.Remove(scanReportPath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("Warning: failed to remove applied scan report %s: %v\n", scanReportPath, err)
		}

		if concerning {
			os.Exit(1)
		}
		return
	}

	if mode == "update" || mode == "add-missing" || mode == "remove-deleted" {
		if err := saveChecksumDB(dbFilePath, checksumDB, verbose); err != nil {
			fmt.Printf("Error saving database: %v\n", err)
			os.Exit(1)
		}
	}

	if mismatchCount > 0 {
		os.Exit(1)
	}
}

// buildScanReport resolves moves and turns an accumulated sync run into the
// report that apply consumes.
func buildScanReport(state *syncState, checksumDB *ChecksumDB, dbFilePath string, roots []string, strictMoves, partial bool) *ScanReport {
	moves, deleted, added, ambiguous := resolveMoves(state.disappeared, state.discovered, checksumDB, strictMoves)

	absDB, err := filepath.Abs(dbFilePath)
	if err != nil {
		absDB = dbFilePath
	}
	dbSize, dbModTime := dbFingerprint(dbFilePath)

	return &ScanReport{
		GeneratedAt:  time.Now().Unix(),
		DBPath:       absDB,
		DBSize:       dbSize,
		DBModTime:    dbModTime,
		Partial:      partial,
		Roots:        roots,
		Verified:     state.verified,
		Ambiguous:    ambiguous,
		Touched:      state.touched,
		Modified:     state.modified,
		Corrupt:      state.corrupt,
		Unverifiable: state.unverifiable,
		Moved:        moves,
		New:          added,
		Deleted:      deleted,
	}
}

func printScanSummary(report *ScanReport, listAll, silent bool) {
	// A scan with nothing to decide on, with nobody watching, says nothing at
	// all -- not even a blank line -- so that the arrival of mail is itself the
	// signal. The exit code still carries the result for the caller.
	if silent {
		return
	}
	// Listing every move and addition buries the findings that need a decision
	// under the ordinary churn of a growing archive, so they are summarized as
	// counts unless the caller asks for the full listing. This is deliberately
	// not tied to -verbose: wanting a progress bar is not the same as wanting
	// every new file enumerated.
	if listAll {
		for _, move := range report.Moved {
			fmt.Printf("Moved: %s -> %s\n", move.From, move.To)
		}
	}
	for _, path := range report.Deleted {
		fmt.Printf("Deleted: %s\n", path)
	}
	if listAll {
		for _, file := range report.New {
			fmt.Printf("New: %s\n", file.Path)
		}
	}

	if report.Partial {
		fmt.Println("\nWARNING: the scan was interrupted and is incomplete.")
	}

	fmt.Println("\nScan summary:")
	fmt.Printf("  verified:     %d\n", report.Verified)
	fmt.Printf("  metadata:     %d\n", len(report.Touched))
	fmt.Printf("  modified:     %d\n", len(report.Modified))
	fmt.Printf("  CORRUPT:      %d\n", len(report.Corrupt))
	fmt.Printf("  unverifiable: %d\n", len(report.Unverifiable))
	fmt.Printf("  moved:        %d\n", len(report.Moved))
	fmt.Printf("  new:          %d\n", len(report.New))
	fmt.Printf("  deleted:      %d\n", len(report.Deleted))
	if report.Ambiguous > 0 {
		fmt.Printf("  ambiguous:    %d (reported as new/deleted instead of moved)\n", report.Ambiguous)
	}

	if report.Concerning() == 0 {
		fmt.Println("\nNothing worrying: no changed, corrupt, unverifiable or deleted files.")
		return
	}
	fmt.Printf("\nNeeds attention: %d changed, %d corrupt, %d unverifiable, %d deleted.\n",
		len(report.Modified), len(report.Corrupt), len(report.Unverifiable), len(report.Deleted))
	if !listAll && (len(report.Moved) > 0 || len(report.New) > 0) {
		fmt.Println("Moves and additions were not listed; pass -list-all to see them.")
	}
}

func printApplyStats(stats applyStats) {
	fmt.Println("\nApplied:")
	fmt.Printf("  metadata recorded: %d\n", stats.touched)
	fmt.Printf("  modified:          %d\n", stats.modified)
	fmt.Printf("  moved:             %d\n", stats.moved)
	fmt.Printf("  added:             %d\n", stats.added)
	fmt.Printf("  removed:           %d\n", stats.removed)
	if stats.accepted > 0 {
		fmt.Printf("  accepted suspects: %d\n", stats.accepted)
	}
	if stats.stale > 0 {
		fmt.Printf("  skipped as stale:  %d\n", stats.stale)
	}
}

// runApply is the standalone apply mode: it turns a previous scan's findings
// into database writes using only stat calls, never re-reading file contents.
func runApply(dbFilePath, scanReportPath string, checksumDB *ChecksumDB, opts applyOpts, force, verbose bool) error {
	report, err := loadScanReport(scanReportPath)
	if err != nil {
		return err
	}
	if err := verifyScanReport(report, dbFilePath, force); err != nil {
		return err
	}

	if len(report.Corrupt) > 0 && !opts.withCorrupt {
		fmt.Printf("Note: %d suspected-corrupt entries left untouched; pass -with-corrupt to accept them.\n", len(report.Corrupt))
	}
	if len(report.Deleted) > 0 && !opts.withDeleted {
		fmt.Printf("Note: %d deleted entries left in the database; pass -with-deleted to remove them.\n", len(report.Deleted))
	}

	stats := applyScanReport(checksumDB, report, opts)
	printApplyStats(stats)

	if err := saveChecksumDB(dbFilePath, checksumDB, verbose); err != nil {
		return fmt.Errorf("failed to save database: %w", err)
	}

	if err := os.Remove(scanReportPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("Warning: failed to remove applied scan report %s: %v\n", scanReportPath, err)
	}

	return nil
}

// formatDuration formats a time.Duration into a human-readable string
// without decimal places, showing hours, minutes, and seconds as relevant.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	hours := d / time.Hour
	d -= hours * time.Hour
	minutes := d / time.Minute
	d -= minutes * time.Minute
	seconds := d / time.Second
	if hours > 0 {
		return fmt.Sprintf("%dh%dm%ds", hours, minutes, seconds)
	} else if minutes > 0 {
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	} else {
		return fmt.Sprintf("%ds", seconds)
	}
}
