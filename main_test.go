package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func missingPath(t *testing.T, elems ...string) string {
	t.Helper()

	parts := append([]string{t.TempDir()}, elems...)
	return filepath.Join(parts...)
}

func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("Failed to create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("Failed to write %s: %v", path, err)
	}
}

func setHermeticHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	return home
}

func hermeticCommand(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()

	home := t.TempDir()
	cmd := exec.Command(name, args...)
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		switch {
		case strings.HasPrefix(entry, "HOME="),
			strings.HasPrefix(entry, "XDG_CONFIG_HOME="),
			strings.HasPrefix(entry, "XDG_DATA_HOME="):
			continue
		default:
			env = append(env, entry)
		}
	}
	env = append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
	)
	cmd.Env = env
	return cmd
}

func TestCalculateChecksum(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "testfile.txt")
	content := []byte("Hello, world!")
	writeTestFile(t, tempFile, content)

	checksum, err := calculateChecksum(tempFile)
	if err != nil {
		t.Fatalf("Failed to calculate checksum: %v", err)
	}

	expectedChecksum := uint64(17691043854468224118)
	if checksum != expectedChecksum {
		t.Errorf("Checksum mismatch. Expected: %d, Got: %d", expectedChecksum, checksum)
	}
}

func TestWorker(t *testing.T) {
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "testfile.txt")
	writeTestFile(t, tempFile, []byte("worker"))

	jobs := make(chan string, 1)
	results := make(chan WorkerResult, 1)
	var wg sync.WaitGroup

	jobs <- tempFile
	close(jobs)

	wg.Add(1)
	go worker(jobs, results, true, &wg)

	wg.Wait()

	result := <-results

	if result.FilePath != tempFile {
		t.Errorf("Expected file path %s, got %s", tempFile, result.FilePath)
	}
	if !result.Exists {
		t.Error("Expected Exists to be true for an existing file")
	}
	if result.Err != nil {
		t.Errorf("Expected no error, got %v", result.Err)
	}
}

func TestWorkerMissingFile(t *testing.T) {
	missingFile := missingPath(t, "missing", "file.txt")
	jobs := make(chan string, 1)
	results := make(chan WorkerResult, 1)
	var wg sync.WaitGroup

	jobs <- missingFile
	close(jobs)

	wg.Add(1)
	go worker(jobs, results, true, &wg)

	wg.Wait()

	result := <-results

	if result.FilePath != missingFile {
		t.Errorf("Expected file path %s, got %s", missingFile, result.FilePath)
	}
	if result.Exists {
		t.Error("Expected Exists to be false for a missing file")
	}
}

func TestWorkerError(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "not-a-regular-file")
	if err := os.Mkdir(tempFile, 0o755); err != nil {
		t.Fatalf("Failed to create directory fixture: %v", err)
	}

	jobs := make(chan string, 1)
	results := make(chan WorkerResult, 1)
	var wg sync.WaitGroup

	jobs <- tempFile
	close(jobs)

	wg.Add(1)
	go worker(jobs, results, true, &wg)

	wg.Wait()

	result := <-results

	if result.Err == nil {
		t.Error("Expected an error for a directory entry")
	}
}

func TestLoadChecksumDB(t *testing.T) {
	tempDir := t.TempDir()
	file1Path := filepath.Join(tempDir, "file1")
	file2Path := filepath.Join(tempDir, "file2")

	dbPath := filepath.Join(t.TempDir(), "checksums.json")
	content := []byte(fmt.Sprintf(`{"checksums":{"%s":1234,"%s":5678}}`, file1Path, file2Path))
	writeTestFile(t, dbPath, content)

	checksumDB, err := loadChecksumDB(dbPath, false)
	if err != nil {
		t.Fatalf("Failed to load checksum database: %v", err)
	}

	expectedChecksums := map[string]uint64{
		file1Path: 1234,
		file2Path: 5678,
	}

	if !reflect.DeepEqual(checksumDB.Checksums, expectedChecksums) {
		t.Errorf("Checksum database mismatch. Expected: %v, Got: %v", expectedChecksums, checksumDB.Checksums)
	}
}

func TestLoadChecksumDBMissing(t *testing.T) {
	checksumDB, err := loadChecksumDB(missingPath(t, "missing", "db.json"), false)
	if err != nil {
		t.Fatalf("Expected no error for missing DB file, got: %v", err)
	}
	if len(checksumDB.Checksums) != 0 {
		t.Errorf("Expected empty checksums map, got %d entries", len(checksumDB.Checksums))
	}
}

func TestLoadChecksumDBCorrupt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "corrupt.json")
	writeTestFile(t, dbPath, []byte(`{invalid json!!!`))

	_, err := loadChecksumDB(dbPath, false)
	if err == nil {
		t.Error("Expected an error for corrupt database file, got nil")
	}
}

func TestLoadChecksumDBNormalizesRelativePaths(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "file.txt")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}

	relPath, err := filepath.Rel(wd, filePath)
	if err != nil {
		t.Fatalf("Failed to create relative path: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "legacy.json")
	content := []byte(fmt.Sprintf(`{"checksums":{"%s":1234}}`, relPath))
	writeTestFile(t, dbPath, content)

	checksumDB, err := loadChecksumDB(dbPath, false)
	if err != nil {
		t.Fatalf("Failed to load checksum database: %v", err)
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		t.Fatalf("Failed to resolve absolute path: %v", err)
	}

	expectedChecksums := map[string]uint64{absPath: 1234}
	if !reflect.DeepEqual(checksumDB.Checksums, expectedChecksums) {
		t.Errorf("Checksum database mismatch. Expected: %v, Got: %v", expectedChecksums, checksumDB.Checksums)
	}
}

func TestGetFilesToProcess(t *testing.T) {
	tempDir := t.TempDir()
	file1 := filepath.Join(tempDir, "file1.txt")
	file2 := filepath.Join(tempDir, "file2.txt")
	writeTestFile(t, file1, []byte("file1"))
	writeTestFile(t, file2, []byte("file2"))

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			file1: 1234,
		},
	}

	// Test case 1: "check" mode
	files, calculateChecksums, err := getFilesToProcess("check", []string{tempDir}, checksumDB)
	if err != nil {
		t.Errorf("Unexpected error in 'check' mode: %v", err)
	}
	expectedFiles := []string{file1}
	if !reflect.DeepEqual(files, expectedFiles) {
		t.Errorf("File list mismatch in 'check' mode. Expected: %v, Got: %v", expectedFiles, files)
	}
	if !calculateChecksums {
		t.Error("Expected calculateChecksums to be true in 'check' mode")
	}

	// Test case 2: "update" mode
	files, calculateChecksums, err = getFilesToProcess("update", []string{tempDir}, checksumDB)
	if err != nil {
		t.Errorf("Unexpected error in 'update' mode: %v", err)
	}
	expectedFiles = []string{file1}
	if !reflect.DeepEqual(files, expectedFiles) {
		t.Errorf("File list mismatch in 'update' mode. Expected: %v, Got: %v", expectedFiles, files)
	}
	if !calculateChecksums {
		t.Error("Expected calculateChecksums to be true in 'update' mode")
	}

	// Test case 3: "list-missing" mode — only file2 is missing from DB
	files, calculateChecksums, err = getFilesToProcess("list-missing", []string{tempDir}, checksumDB)
	if err != nil {
		t.Errorf("Unexpected error in 'list-missing' mode: %v", err)
	}
	expectedFiles = []string{file2}
	if !reflect.DeepEqual(files, expectedFiles) {
		t.Errorf("File list mismatch in 'list-missing' mode. Expected: %v, Got: %v", expectedFiles, files)
	}
	if calculateChecksums {
		t.Error("Expected calculateChecksums to be false in 'list-missing' mode")
	}

	// Test case 4: "add-missing" mode
	files, calculateChecksums, err = getFilesToProcess("add-missing", []string{tempDir}, checksumDB)
	if err != nil {
		t.Errorf("Unexpected error in 'add-missing' mode: %v", err)
	}
	expectedFiles = []string{file2}
	if !reflect.DeepEqual(files, expectedFiles) {
		t.Errorf("File list mismatch in 'add-missing' mode. Expected: %v, Got: %v", expectedFiles, files)
	}
	if !calculateChecksums {
		t.Error("Expected calculateChecksums to be true in 'add-missing' mode")
	}

	// Test case 5: "remove-deleted" mode
	files, calculateChecksums, err = getFilesToProcess("remove-deleted", []string{tempDir}, checksumDB)
	if err != nil {
		t.Errorf("Unexpected error in 'remove-deleted' mode: %v", err)
	}
	expectedFiles = []string{file1}
	if !reflect.DeepEqual(files, expectedFiles) {
		t.Errorf("File list mismatch in 'remove-deleted' mode. Expected: %v, Got: %v", expectedFiles, files)
	}
	if calculateChecksums {
		t.Error("Expected calculateChecksums to be false in 'remove-deleted' mode")
	}

	// Test case 6: invalid mode
	_, _, err = getFilesToProcess("invalid", []string{tempDir}, checksumDB)
	if err == nil {
		t.Error("Expected an error for invalid mode, but got nil")
	}
}

func TestGetFilesToProcessDirectoryFilter(t *testing.T) {
	rootDir := t.TempDir()
	tempDir1 := filepath.Join(rootDir, "dir1")
	tempDir2 := filepath.Join(rootDir, "dir2")
	if err := os.MkdirAll(tempDir1, 0o755); err != nil {
		t.Fatalf("Failed to create directory %s: %v", tempDir1, err)
	}
	if err := os.MkdirAll(tempDir2, 0o755); err != nil {
		t.Fatalf("Failed to create directory %s: %v", tempDir2, err)
	}

	absDir1, _ := filepath.Abs(tempDir1)
	absDir2, _ := filepath.Abs(tempDir2)
	file1Path := filepath.Join(absDir1, "file1.txt")
	file2Path := filepath.Join(absDir2, "file2.txt")

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			file1Path: 1234,
			file2Path: 5678,
		},
	}

	// check mode with only dir1 should exclude file2
	files, _, err := getFilesToProcess("check", []string{tempDir1}, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(files) != 1 || files[0] != file1Path {
		t.Errorf("Expected only %s, got %v", file1Path, files)
	}

	// check mode with no directories should include all
	files, _, err = getFilesToProcess("check", nil, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	sort.Strings(files)
	expected := []string{file1Path, file2Path}
	sort.Strings(expected)
	if !reflect.DeepEqual(files, expected) {
		t.Errorf("Expected %v, got %v", expected, files)
	}
}

func TestSaveChecksumDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "checksums.json")

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			"file1": 1234,
			"file2": 5678,
		},
	}

	err := saveChecksumDB(dbPath, checksumDB, false)
	if err != nil {
		t.Fatalf("Failed to save checksum database: %v", err)
	}

	content, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("Failed to read saved checksum database file: %v", err)
	}

	var savedDB map[string]map[string]uint64
	err = json.Unmarshal(content, &savedDB)
	if err != nil {
		t.Fatalf("Failed to parse saved checksum database: %v", err)
	}

	expectedDB := map[string]map[string]uint64{
		"checksums": {
			"file1": 1234,
			"file2": 5678,
		},
	}

	if !reflect.DeepEqual(savedDB, expectedDB) {
		t.Errorf("Saved checksum database content mismatch. Expected: %v, Got: %v", expectedDB, savedDB)
	}
}

func TestSaveChecksumDBPermissions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sub", "checksums.json")
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}

	err := saveChecksumDB(dbPath, checksumDB, false)
	if err != nil {
		t.Fatalf("Failed to save checksum database: %v", err)
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("Failed to stat saved file: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("Expected file permissions 0600, got %04o", perm)
	}
}

func TestProcessResults(t *testing.T) {
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			"file1": 1234,
			"file2": 5678,
		},
	}

	var outputMu sync.Mutex

	// Test case 1: "check" mode
	results := make(chan WorkerResult, 3)
	done := make(chan struct{})
	var processedFiles uint64

	results <- WorkerResult{FilePath: "file1", Checksum: 1234, Exists: true}
	results <- WorkerResult{FilePath: "file2", Checksum: 0, Exists: false}
	results <- WorkerResult{FilePath: "file3", Checksum: 9012, Exists: true}
	close(results)

	mismatchCount := processResults(results, done, "check", checksumDB, &processedFiles, &outputMu, "")
	<-done

	if mismatchCount != 1 {
		t.Errorf("Expected 1 mismatch in check mode (missing file2), got %d", mismatchCount)
	}

	// Test case 2: "update" mode
	results = make(chan WorkerResult, 2)
	done = make(chan struct{})
	processedFiles = 0

	results <- WorkerResult{FilePath: "file1", Checksum: 1234, Exists: true}
	results <- WorkerResult{FilePath: "file3", Checksum: 9012, Exists: true}
	close(results)

	processResults(results, done, "update", checksumDB, &processedFiles, &outputMu, "")
	<-done

	if checksumDB.Checksums["file3"] != 9012 {
		t.Error("New file should have been added to the checksum database")
	}

	// Test case 3: "list-missing" mode
	results = make(chan WorkerResult, 2)
	done = make(chan struct{})
	processedFiles = 0

	results <- WorkerResult{FilePath: "file1", Checksum: 0, Exists: true}
	results <- WorkerResult{FilePath: "file4", Checksum: 0, Exists: true}
	close(results)

	processResults(results, done, "list-missing", checksumDB, &processedFiles, &outputMu, "")
	<-done

	// Test case 4: "add-missing" mode
	results = make(chan WorkerResult, 2)
	done = make(chan struct{})
	processedFiles = 0

	results <- WorkerResult{FilePath: "file4", Checksum: 3456, Exists: true}
	results <- WorkerResult{FilePath: "file5", Checksum: 0, Exists: false}
	close(results)

	processResults(results, done, "add-missing", checksumDB, &processedFiles, &outputMu, "")
	<-done

	if checksumDB.Checksums["file4"] != 3456 {
		t.Error("New file should have been added to the checksum database")
	}

	// Test case 5: "remove-deleted" mode
	results = make(chan WorkerResult, 2)
	done = make(chan struct{})
	processedFiles = 0

	results <- WorkerResult{FilePath: "file1", Checksum: 0, Exists: false}
	results <- WorkerResult{FilePath: "file2", Checksum: 0, Exists: false}
	close(results)

	processResults(results, done, "remove-deleted", checksumDB, &processedFiles, &outputMu, "")
	<-done

	if _, ok := checksumDB.Checksums["file1"]; ok {
		t.Error("Missing file should have been removed from the checksum database")
	}
	if _, ok := checksumDB.Checksums["file2"]; ok {
		t.Error("Missing file should have been removed from the checksum database")
	}
}

func TestProcessResultsMismatchCount(t *testing.T) {
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			"file1": 1234,
			"file2": 5678,
		},
	}
	var outputMu sync.Mutex

	results := make(chan WorkerResult, 3)
	done := make(chan struct{})
	var processedFiles uint64

	// file1: matches, file2: mismatch, file3: missing
	results <- WorkerResult{FilePath: "file1", Checksum: 1234, Exists: true}
	results <- WorkerResult{FilePath: "file2", Checksum: 9999, Exists: true}
	results <- WorkerResult{FilePath: "file3", Checksum: 0, Exists: false}
	close(results)

	count := processResults(results, done, "check", checksumDB, &processedFiles, &outputMu, "")
	<-done

	if count != 2 {
		t.Errorf("Expected 2 mismatches (1 checksum mismatch + 1 missing), got %d", count)
	}
}

func TestFileExists(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "testfile.txt")
	writeTestFile(t, tempFile, []byte("exists"))

	if !fileExists(tempFile) {
		t.Error("Expected fileExists to return true for an existing file")
	}

	if fileExists(missingPath(t, "missing", "file.txt")) {
		t.Error("Expected fileExists to return false for a nonexistent file")
	}
}

func TestIsUnderDir(t *testing.T) {
	tests := []struct {
		filePath string
		dirPath  string
		expected bool
	}{
		{"/home/user/docs/file.txt", "/home/user/docs", true},
		{"/home/user/docs/sub/file.txt", "/home/user/docs", true},
		{"/home/user/documents/file.txt", "/home/user/docs", false},
		{"/home/user/doc", "/home/user/docs", false},
		{"/home/user/docs", "/home/user/docs", true},
		{"/other/path/file.txt", "/home/user/docs", false},
	}

	for _, tc := range tests {
		result := isUnderDir(tc.filePath, tc.dirPath)
		if result != tc.expected {
			t.Errorf("isUnderDir(%q, %q) = %v, expected %v", tc.filePath, tc.dirPath, result, tc.expected)
		}
	}
}

func TestWalkDirectoriesForMissing(t *testing.T) {
	tempDir := t.TempDir()
	file1 := filepath.Join(tempDir, "file1.txt")
	file2 := filepath.Join(tempDir, "file2.txt")
	writeTestFile(t, file1, []byte("file1"))
	writeTestFile(t, file2, []byte("file2"))

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			file1: 1234,
		},
	}

	files, err := walkDirectoriesForMissing([]string{tempDir}, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(files) != 1 || files[0] != file2 {
		t.Errorf("Expected [%s], got %v", file2, files)
	}
}

func TestWalkDirectoriesForMissingRootSymlinkDir(t *testing.T) {
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "target")
	filePath := filepath.Join(targetDir, "file.txt")
	writeTestFile(t, filePath, []byte("file"))

	linkPath := filepath.Join(tempDir, "link")
	if err := os.Symlink(targetDir, linkPath); err != nil {
		t.Fatalf("Failed to create symlinked directory: %v", err)
	}

	checksumDB := &ChecksumDB{Checksums: make(map[string]uint64)}
	files, err := walkDirectoriesForMissing([]string{linkPath}, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	absFile, err := filepath.Abs(filePath)
	if err != nil {
		t.Fatalf("Failed to resolve absolute file path: %v", err)
	}
	if !reflect.DeepEqual(files, []string{absFile}) {
		t.Fatalf("Expected only target file %s, got %v", absFile, files)
	}
}

func TestFormatDuration(t *testing.T) {
	testCases := []struct {
		duration time.Duration
		expected string
	}{
		{time.Second * 30, "30s"},
		{time.Minute * 2, "2m0s"},
		{time.Hour*1 + time.Minute*30 + time.Second*15, "1h30m15s"},
	}

	for _, tc := range testCases {
		result := formatDuration(tc.duration)
		if result != tc.expected {
			t.Errorf("Formatted duration mismatch. Expected: %s, Got: %s", tc.expected, result)
		}
	}
}

func TestCalculateChecksumNonexistent(t *testing.T) {
	_, err := calculateChecksum(missingPath(t, "missing", "file.txt"))
	if err == nil {
		t.Error("Expected an error for nonexistent file")
	}
}

func TestLoadChecksumDBVerbose(t *testing.T) {
	// Test verbose with existing DB
	dbPath := filepath.Join(t.TempDir(), "checksums.json")
	writeTestFile(t, dbPath, []byte(`{"checksums":{"file1":1234}}`))

	db, err := loadChecksumDB(dbPath, true)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(db.Checksums) != 1 {
		t.Errorf("Expected 1 entry, got %d", len(db.Checksums))
	}

	// Test verbose with missing DB
	db, err = loadChecksumDB(missingPath(t, "missing", "db.json"), true)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(db.Checksums) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(db.Checksums))
	}
}

func TestSaveChecksumDBVerbose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "checksums.json")

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}

	err := saveChecksumDB(dbPath, checksumDB, true)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
}

func TestSaveChecksumDBWriteError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	writeTestFile(t, blocker, []byte("not a directory"))

	err := saveChecksumDB(filepath.Join(blocker, "db.json"), &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}, false)
	if err == nil {
		t.Error("Expected an error when the database parent path is a file")
	}
}

func TestProcessResultsListDeleted(t *testing.T) {
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			"file1": 1234,
			"file2": 5678,
		},
	}
	var outputMu sync.Mutex

	results := make(chan WorkerResult, 3)
	done := make(chan struct{})
	var processedFiles uint64

	results <- WorkerResult{FilePath: "file1", Checksum: 0, Exists: false}
	results <- WorkerResult{FilePath: "file2", Checksum: 0, Exists: true}
	close(results)

	processResults(results, done, "list-deleted", checksumDB, &processedFiles, &outputMu, "")
	<-done

	// list-deleted should not modify the DB
	if _, ok := checksumDB.Checksums["file1"]; !ok {
		t.Error("list-deleted should not remove entries from DB")
	}
}

func TestProcessResultsWithError(t *testing.T) {
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}
	var outputMu sync.Mutex

	results := make(chan WorkerResult, 1)
	done := make(chan struct{})
	var processedFiles uint64

	results <- WorkerResult{FilePath: "file1", Exists: true, Err: os.ErrPermission}
	close(results)

	count := processResults(results, done, "check", checksumDB, &processedFiles, &outputMu, "")
	<-done

	if count != 1 {
		t.Errorf("Expected 1 mismatch for error result, got %d", count)
	}
}

func TestLoadChecksumDBUnreadable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "db-dir")
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatalf("Failed to create directory fixture: %v", err)
	}

	_, err := loadChecksumDB(dbPath, false)
	if err == nil {
		t.Error("Expected an error for a database path that is a directory")
	}
}

func TestSaveChecksumDBTargetIsDirectory(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "checksums.json")
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatalf("Failed to create directory fixture: %v", err)
	}

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}
	err := saveChecksumDB(dbPath, checksumDB, false)
	if err == nil {
		t.Error("Expected an error when the database target path is a directory")
	}
}

func TestGetFilesToProcessDeletedDirectoryFilter(t *testing.T) {
	rootDir := t.TempDir()
	absDir1 := filepath.Join(rootDir, "testdir_a")
	absDir2 := filepath.Join(rootDir, "testdir_b")
	file1Path := filepath.Join(absDir1, "file1.txt")
	file2Path := filepath.Join(absDir2, "file2.txt")

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			file1Path: 1234,
			file2Path: 5678,
		},
	}

	// remove-deleted with directory filter
	files, calc, err := getFilesToProcess("remove-deleted", []string{absDir1}, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if calc {
		t.Error("Expected calculateChecksums to be false for remove-deleted")
	}
	if len(files) != 1 || files[0] != file1Path {
		t.Errorf("Expected [%s], got %v", file1Path, files)
	}

	// list-deleted with no directories should include all
	files, _, err = getFilesToProcess("list-deleted", nil, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	sort.Strings(files)
	expected := []string{file1Path, file2Path}
	sort.Strings(expected)
	if !reflect.DeepEqual(files, expected) {
		t.Errorf("Expected %v, got %v", expected, files)
	}

	// list-deleted with directory filter
	files, _, err = getFilesToProcess("list-deleted", []string{absDir2}, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(files) != 1 || files[0] != file2Path {
		t.Errorf("Expected [%s], got %v", file2Path, files)
	}
}

func TestGetFilesToProcessRequiresDirectories(t *testing.T) {
	checksumDB := &ChecksumDB{Checksums: make(map[string]uint64)}

	_, _, err := getFilesToProcess("list-missing", nil, checksumDB)
	if err == nil {
		t.Error("Expected error for list-missing without directories")
	}

	_, _, err = getFilesToProcess("add-missing", nil, checksumDB)
	if err == nil {
		t.Error("Expected error for add-missing without directories")
	}
}

func TestWalkDirectoriesForMissingError(t *testing.T) {
	checksumDB := &ChecksumDB{Checksums: make(map[string]uint64)}
	_, err := walkDirectoriesForMissing([]string{missingPath(t, "missing", "directory")}, checksumDB)
	if err == nil {
		t.Error("Expected an error for nonexistent directory")
	}
}

func TestDefaultDBPath(t *testing.T) {
	home := setHermeticHome(t)
	path := defaultDBPath()
	expected := filepath.Join(home, ".local", "share", "checksumtool", "checksums.json")
	if path != expected {
		t.Errorf("Expected defaultDBPath to use hermetic home.\nexpected: %s\ngot: %s", expected, path)
	}
}

func TestUpdateProgressBar(t *testing.T) {
	done := make(chan struct{})
	var processedFiles uint64
	var outputMu sync.Mutex
	startTime := time.Now()

	atomic.StoreUint64(&processedFiles, 5)

	go updateProgressBar(done, 10, &processedFiles, startTime, &outputMu)

	// Let the ticker fire at least once
	time.Sleep(1100 * time.Millisecond)
	close(done)

	// Give it time to return
	time.Sleep(100 * time.Millisecond)
}

func TestWorkerNoChecksum(t *testing.T) {
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "testfile.txt")
	writeTestFile(t, tempFile, []byte("content"))

	jobs := make(chan string, 1)
	results := make(chan WorkerResult, 1)
	var wg sync.WaitGroup

	jobs <- tempFile
	close(jobs)

	wg.Add(1)
	go worker(jobs, results, false, &wg)
	wg.Wait()

	result := <-results
	if result.Checksum != 0 {
		t.Errorf("Expected checksum 0 when calculateChecksums=false, got %d", result.Checksum)
	}
	if !result.Exists {
		t.Error("Expected Exists to be true")
	}
}

func TestMainBinary(t *testing.T) {
	// Build the binary
	binPath := filepath.Join(t.TempDir(), "checksumtool")
	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("Failed to build binary: %v\n%s", err, out)
	}

	// Test: no mode flag
	cmd := hermeticCommand(t, binPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for missing -mode flag")
	}
	if !strings.Contains(string(out), "operation mode") {
		t.Errorf("Expected mode error message, got: %s", out)
	}

	// Test: invalid workers
	cmd = hermeticCommand(t, binPath, "-workers", "0", "-mode", "check")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for -workers 0")
	}
	if !strings.Contains(string(out), "workers") {
		t.Errorf("Expected workers error message, got: %s", out)
	}

	// Test: list-missing without directories
	cmd = hermeticCommand(t, binPath, "-mode", "list-missing")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for list-missing without dirs")
	}
	if !strings.Contains(string(out), "requires") {
		t.Errorf("Expected directory requirement message, got: %s", out)
	}

	// Test: add-missing without directories
	cmd = hermeticCommand(t, binPath, "-mode", "add-missing")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for add-missing without dirs")
	}

	// Test: check mode with no DB (should succeed, 0 files)
	dbPath := filepath.Join(t.TempDir(), "test.json")
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", dbPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected success for check with empty DB, got: %v\n%s", err, out)
	}

	// Test: corrupt DB file
	corruptDB := filepath.Join(t.TempDir(), "corrupt.json")
	writeTestFile(t, corruptDB, []byte("{bad json!"))
	if err := os.Chmod(corruptDB, 0o600); err != nil {
		t.Fatalf("Failed to chmod corrupt DB: %v", err)
	}
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", corruptDB)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for corrupt DB")
	}
	if !strings.Contains(string(out), "Error loading database") {
		t.Errorf("Expected DB error message, got: %s", out)
	}

	// Test: add-missing populates DB, check detects mismatch
	testDir := t.TempDir()
	testFile := filepath.Join(testDir, "hello.txt")
	writeTestFile(t, testFile, []byte("hello"))
	freshDB := filepath.Join(t.TempDir(), "fresh.json")

	cmd = hermeticCommand(t, binPath, "-mode", "add-missing", "-db", freshDB, testDir)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected success for add-missing, got: %v\n%s", err, out)
	}

	// Verify DB was created
	if !fileExists(freshDB) {
		t.Fatal("Expected DB file to be created")
	}

	// Check should pass
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", freshDB, testDir)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected check to pass, got: %v\n%s", err, out)
	}

	// Modify file, check should fail with exit 1
	writeTestFile(t, testFile, []byte("modified"))
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", freshDB, testDir)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for checksum mismatch")
	}
	if !strings.Contains(string(out), "Mismatch") {
		t.Errorf("Expected mismatch message, got: %s", out)
	}

	// Test: verbose mode
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", freshDB, "-verbose", testDir)
	out, err = cmd.CombinedOutput()
	// Will exit 1 due to mismatch, but should have verbose output
	if !strings.Contains(string(out), "Loading checksum database") {
		t.Errorf("Expected verbose output, got: %s", out)
	}

	// Test: update mode with a database entry that points at a directory
	// should exit non-zero without relying on production paths or chmod.
	errDir := t.TempDir()
	errPath := filepath.Join(errDir, "dir-entry")
	if err := os.Mkdir(errPath, 0o755); err != nil {
		t.Fatalf("Failed to create directory fixture: %v", err)
	}
	errDB := filepath.Join(t.TempDir(), "err.json")
	writeTestFile(t, errDB, []byte(fmt.Sprintf(`{"checksums":{"%s":1234}}`, errPath)))

	cmd = hermeticCommand(t, binPath, "-mode", "update", "-db", errDB, errDir)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for update mode when a DB entry points at a directory")
	}
	if !strings.Contains(string(out), "Error processing file") {
		t.Errorf("Expected update error output, got: %s", out)
	}

	// Test: config file provides directories
	cfgDir := t.TempDir()
	cfgTestDir := t.TempDir()
	cfgTestFile := filepath.Join(cfgTestDir, "cfgfile.txt")
	writeTestFile(t, cfgTestFile, []byte("config test"))
	cfgDB := filepath.Join(t.TempDir(), "cfg.json")
	cfgFile := filepath.Join(cfgDir, "config.toml")
	writeTestFile(t, cfgFile, []byte(fmt.Sprintf("directories = [%q]\n", cfgTestDir)))

	// add-missing using config file directories (no CLI dirs)
	cmd = hermeticCommand(t, binPath, "-mode", "add-missing", "-db", cfgDB, "-config", cfgFile)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected success for add-missing with config dirs, got: %v\n%s", err, out)
	}
	if !fileExists(cfgDB) {
		t.Fatal("Expected DB file to be created from config dirs")
	}

	// check using config file directories should pass
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", cfgDB, "-config", cfgFile)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected check to pass with config dirs, got: %v\n%s", err, out)
	}

	// Test: missing config file should silently use defaults
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", dbPath, "-config", missingPath(t, "missing", "config.toml"))
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected success with missing config file, got: %v\n%s", err, out)
	}

	// Test: invalid config file should error
	badCfg := filepath.Join(t.TempDir(), "bad.toml")
	writeTestFile(t, badCfg, []byte("{{invalid!!"))
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", dbPath, "-config", badCfg)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for invalid config file")
	}

	// Test: config with workers and verbose
	cfgWithOpts := filepath.Join(t.TempDir(), "opts.toml")
	writeTestFile(t, cfgWithOpts, []byte("workers = 2\nverbose = true\n"))
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", cfgDB, "-config", cfgWithOpts)
	out, err = cmd.CombinedOutput()
	// Should have verbose output from config
	if !strings.Contains(string(out), "Loading checksum database") {
		t.Errorf("Expected verbose output from config, got: %s", out)
	}

	// Test: CLI flags override config
	cmd = hermeticCommand(t, binPath, "-mode", "check", "-db", cfgDB, "-config", cfgWithOpts, "-verbose=false")
	out, err = cmd.CombinedOutput()
	if strings.Contains(string(out), "Loading checksum database") {
		t.Errorf("Expected CLI -verbose=false to override config, got: %s", out)
	}

	// Regression test: list-deleted should still work when the DB contains
	// a legacy relative path and the user scopes the check to a directory.
	legacyDir := t.TempDir()
	legacyFile := filepath.Join(legacyDir, "legacy.txt")
	writeTestFile(t, legacyFile, []byte("legacy"))

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}

	legacyRelativePath, err := filepath.Rel(wd, legacyFile)
	if err != nil {
		t.Fatalf("Failed to compute legacy relative path: %v", err)
	}

	legacyDB := filepath.Join(t.TempDir(), "legacy.json")
	legacyJSON := fmt.Sprintf(`{"checksums":{"%s":1234}}`, legacyRelativePath)
	writeTestFile(t, legacyDB, []byte(legacyJSON))
	if err := os.Chmod(legacyDB, 0o600); err != nil {
		t.Fatalf("Failed to chmod legacy DB: %v", err)
	}

	if err := os.Remove(legacyFile); err != nil {
		t.Fatalf("Failed to delete legacy test file: %v", err)
	}

	cmd = hermeticCommand(t, binPath, "-mode", "list-deleted", "-db", legacyDB, legacyDir)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected success for list-deleted with legacy DB entry, got: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "File deleted:") {
		t.Errorf("Expected deleted file output for legacy DB entry, got: %s", out)
	}

	// Regression test: list-deleted without CLI directories should scan the
	// entire DB even when config directories are present.
	filteredDir := t.TempDir()
	unfilteredDir := t.TempDir()
	filteredFile := filepath.Join(filteredDir, "filtered.txt")
	unfilteredFile := filepath.Join(unfilteredDir, "unfiltered.txt")

	writeTestFile(t, filteredFile, []byte("filtered"))
	writeTestFile(t, unfilteredFile, []byte("unfiltered"))

	configFilteredDB := filepath.Join(t.TempDir(), "filtered.json")
	cmd = hermeticCommand(t, binPath, "-mode", "add-missing", "-db", configFilteredDB, filteredDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to add filtered dir: %v\n%s", err, out)
	}
	cmd = hermeticCommand(t, binPath, "-mode", "add-missing", "-db", configFilteredDB, unfilteredDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to add unfiltered dir: %v\n%s", err, out)
	}

	if err := os.Remove(filteredFile); err != nil {
		t.Fatalf("Failed to remove filtered file: %v", err)
	}
	if err := os.Remove(unfilteredFile); err != nil {
		t.Fatalf("Failed to remove unfiltered file: %v", err)
	}

	configWithDirs := filepath.Join(t.TempDir(), "dirs.toml")
	configBody := fmt.Sprintf("directories = [%q]\n", filteredDir)
	writeTestFile(t, configWithDirs, []byte(configBody))

	cmd = hermeticCommand(t, binPath, "-mode", "list-deleted", "-db", configFilteredDB, "-config", configWithDirs)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected success for list-deleted with config directories, got: %v\n%s", err, out)
	}
	if strings.Count(string(out), "File deleted:") != 2 {
		t.Errorf("Expected both deleted files to be listed despite config directories, got: %s", out)
	}
}

func TestProcessResultsUpdateDeletedFile(t *testing.T) {
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			"file1": 1234,
		},
	}
	var outputMu sync.Mutex

	results := make(chan WorkerResult, 1)
	done := make(chan struct{})
	var processedFiles uint64

	// Simulate a deleted file in update mode
	results <- WorkerResult{FilePath: "file1", Checksum: 0, Exists: false}
	close(results)

	processResults(results, done, "update", checksumDB, &processedFiles, &outputMu, "")
	<-done

	// The DB entry should NOT be modified (should still be 1234, not 0)
	if checksum, ok := checksumDB.Checksums["file1"]; !ok {
		t.Error("Expected file1 to still be in DB after update with missing file")
	} else if checksum != 1234 {
		t.Errorf("Expected checksum 1234 preserved, got %d", checksum)
	}
}

func TestSaveChecksumDBAtomicValidJSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "checksums.json")
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			"file1": 1234,
			"file2": 5678,
		},
	}

	err := saveChecksumDB(dbPath, checksumDB, false)
	if err != nil {
		t.Fatalf("Failed to save: %v", err)
	}

	// Verify the written file is valid JSON
	content, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("Failed to read saved file: %v", err)
	}

	var loaded ChecksumDB
	if err := json.Unmarshal(content, &loaded); err != nil {
		t.Fatalf("Saved file is not valid JSON: %v", err)
	}

	if len(loaded.Checksums) != 2 {
		t.Errorf("Expected 2 entries, got %d", len(loaded.Checksums))
	}
}

func TestLockDBFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "checksums.json")

	// Acquire exclusive lock
	f, err := lockDBFile(dbPath, true)
	if err != nil {
		t.Fatalf("Failed to acquire lock: %v", err)
	}

	// Try to acquire another exclusive lock — should fail (LOCK_NB)
	_, err = lockDBFile(dbPath, true)
	if err == nil {
		t.Error("Expected error when acquiring second exclusive lock")
	}

	// Release first lock
	unlockDBFile(f)

	// Now should succeed
	f2, err := lockDBFile(dbPath, true)
	if err != nil {
		t.Fatalf("Failed to acquire lock after release: %v", err)
	}
	unlockDBFile(f2)
}

func TestLoadConfig(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "config.toml")
	content := `directories = ["/home/user/photos", "/home/user/docs"]
workers = 8
verbose = true
`
	writeTestFile(t, tempFile, []byte(content))

	cfg, err := loadConfig(tempFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if len(cfg.Directories) != 2 {
		t.Errorf("Expected 2 directories, got %d", len(cfg.Directories))
	}
	if cfg.Directories[0] != "/home/user/photos" {
		t.Errorf("Expected /home/user/photos, got %s", cfg.Directories[0])
	}
	if cfg.Directories[1] != "/home/user/docs" {
		t.Errorf("Expected /home/user/docs, got %s", cfg.Directories[1])
	}
	if cfg.Workers != 8 {
		t.Errorf("Expected workers=8, got %d", cfg.Workers)
	}
	if !cfg.Verbose {
		t.Error("Expected verbose=true")
	}
}

func TestLoadConfigMissing(t *testing.T) {
	cfg, err := loadConfig(missingPath(t, "missing", "config.toml"))
	if err != nil {
		t.Fatalf("Expected no error for missing config, got: %v", err)
	}
	if len(cfg.Directories) != 0 {
		t.Errorf("Expected empty directories, got %d", len(cfg.Directories))
	}
	if cfg.Workers != 0 {
		t.Errorf("Expected workers=0, got %d", cfg.Workers)
	}
	if cfg.Verbose {
		t.Error("Expected verbose=false")
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "config.toml")
	writeTestFile(t, tempFile, []byte("{{invalid toml!!"))

	_, err := loadConfig(tempFile)
	if err == nil {
		t.Error("Expected error for invalid TOML, got nil")
	}
}

func TestLoadConfigEmptyPath(t *testing.T) {
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("Expected no error for empty path, got: %v", err)
	}
	if cfg.Workers != 0 {
		t.Errorf("Expected zero-value config, got workers=%d", cfg.Workers)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	home := setHermeticHome(t)
	path := defaultConfigPath()
	expected := filepath.Join(home, ".config", "checksumtool", "config.toml")
	if path != expected {
		t.Errorf("Expected defaultConfigPath to use hermetic home.\nexpected: %s\ngot: %s", expected, path)
	}
}

func TestResolveDirectories(t *testing.T) {
	configDirs := []string{"/config/dir"}
	cliDirs := []string{"/cli/dir"}

	tests := []struct {
		name     string
		mode     string
		cli      []string
		config   []string
		expected []string
	}{
		{
			name:     "cli directories always win",
			mode:     "list-deleted",
			cli:      cliDirs,
			config:   configDirs,
			expected: cliDirs,
		},
		{
			name:     "list missing uses config directories",
			mode:     "list-missing",
			config:   configDirs,
			expected: configDirs,
		},
		{
			name:     "add missing uses config directories",
			mode:     "add-missing",
			config:   configDirs,
			expected: configDirs,
		},
		{
			name:     "list deleted ignores config directories",
			mode:     "list-deleted",
			config:   configDirs,
			expected: nil,
		},
		{
			name:     "check ignores config directories",
			mode:     "check",
			config:   configDirs,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveDirectories(tt.mode, tt.cli, tt.config)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Fatalf("resolveDirectories(%q, %v, %v) = %v, expected %v", tt.mode, tt.cli, tt.config, got, tt.expected)
			}
		})
	}
}

func TestWalkDirectoriesForMissingSymlinkDir(t *testing.T) {
	tempDir := t.TempDir()

	// Create a real file
	realFile := filepath.Join(tempDir, "real.txt")
	writeTestFile(t, realFile, []byte("data"))

	// Create a subdirectory with a file
	subDir := filepath.Join(tempDir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("Failed to create subdirectory: %v", err)
	}
	subFile := filepath.Join(subDir, "sub.txt")
	writeTestFile(t, subFile, []byte("sub"))

	// Create a symlink loop: tempDir/loop -> tempDir
	loopLink := filepath.Join(tempDir, "loop")
	if err := os.Symlink(tempDir, loopLink); err != nil {
		t.Fatalf("Failed to create symlink loop: %v", err)
	}

	checksumDB := &ChecksumDB{Checksums: make(map[string]uint64)}

	// This should not hang (WalkDir doesn't follow symlinks into directories)
	files, err := walkDirectoriesForMissing([]string{tempDir}, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Should find real.txt and subdir/sub.txt, but NOT follow the loop symlink
	absReal, _ := filepath.Abs(realFile)
	absSub, _ := filepath.Abs(subFile)
	found := make(map[string]bool)
	for _, f := range files {
		found[f] = true
	}

	if !found[absReal] {
		t.Errorf("Expected to find %s in results", absReal)
	}
	if !found[absSub] {
		t.Errorf("Expected to find %s in results", absSub)
	}
	if found[loopLink] {
		t.Errorf("Expected symlinked directory %s to be skipped", loopLink)
	}
}
