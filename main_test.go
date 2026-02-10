package main

import (
	"encoding/json"
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

func TestCalculateChecksum(t *testing.T) {
	tempFile, err := os.CreateTemp("", "testfile")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	content := []byte("Hello, world!")
	if _, err := tempFile.Write(content); err != nil {
		t.Fatalf("Failed to write content to temporary file: %v", err)
	}
	tempFile.Close()

	checksum, err := calculateChecksum(tempFile.Name())
	if err != nil {
		t.Fatalf("Failed to calculate checksum: %v", err)
	}

	expectedChecksum := uint64(17691043854468224118)
	if checksum != expectedChecksum {
		t.Errorf("Checksum mismatch. Expected: %d, Got: %d", expectedChecksum, checksum)
	}
}

func TestWorker(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	tempFile, err := os.CreateTemp(tempDir, "testfile")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	tempFile.Close()

	jobs := make(chan string, 1)
	results := make(chan WorkerResult, 1)
	var wg sync.WaitGroup

	jobs <- tempFile.Name()
	close(jobs)

	wg.Add(1)
	go worker(jobs, results, true, &wg)

	wg.Wait()

	result := <-results

	if result.FilePath != tempFile.Name() {
		t.Errorf("Expected file path %s, got %s", tempFile.Name(), result.FilePath)
	}
	if !result.Exists {
		t.Error("Expected Exists to be true for an existing file")
	}
	if result.Err != nil {
		t.Errorf("Expected no error, got %v", result.Err)
	}
}

func TestWorkerMissingFile(t *testing.T) {
	jobs := make(chan string, 1)
	results := make(chan WorkerResult, 1)
	var wg sync.WaitGroup

	jobs <- "/nonexistent/file/path"
	close(jobs)

	wg.Add(1)
	go worker(jobs, results, true, &wg)

	wg.Wait()

	result := <-results

	if result.FilePath != "/nonexistent/file/path" {
		t.Errorf("Expected file path /nonexistent/file/path, got %s", result.FilePath)
	}
	if result.Exists {
		t.Error("Expected Exists to be false for a missing file")
	}
}

func TestWorkerError(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	tempFile, err := os.CreateTemp(tempDir, "testfile")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	tempFile.Close()

	// Make file unreadable
	os.Chmod(tempFile.Name(), 0000)

	jobs := make(chan string, 1)
	results := make(chan WorkerResult, 1)
	var wg sync.WaitGroup

	jobs <- tempFile.Name()
	close(jobs)

	wg.Add(1)
	go worker(jobs, results, true, &wg)

	wg.Wait()

	result := <-results

	if result.Err == nil {
		t.Error("Expected an error for unreadable file")
	}
}

func TestLoadChecksumDB(t *testing.T) {
	tempFile, err := os.CreateTemp("", "testdb")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	content := []byte(`{"checksums":{"file1":1234,"file2":5678}}`)
	if _, err := tempFile.Write(content); err != nil {
		t.Fatalf("Failed to write content to temporary file: %v", err)
	}
	tempFile.Close()

	checksumDB, err := loadChecksumDB(tempFile.Name(), false)
	if err != nil {
		t.Fatalf("Failed to load checksum database: %v", err)
	}

	expectedChecksums := map[string]uint64{
		"file1": 1234,
		"file2": 5678,
	}

	if !reflect.DeepEqual(checksumDB.Checksums, expectedChecksums) {
		t.Errorf("Checksum database mismatch. Expected: %v, Got: %v", expectedChecksums, checksumDB.Checksums)
	}
}

func TestLoadChecksumDBMissing(t *testing.T) {
	checksumDB, err := loadChecksumDB("/nonexistent/path/db.json", false)
	if err != nil {
		t.Fatalf("Expected no error for missing DB file, got: %v", err)
	}
	if len(checksumDB.Checksums) != 0 {
		t.Errorf("Expected empty checksums map, got %d entries", len(checksumDB.Checksums))
	}
}

func TestLoadChecksumDBCorrupt(t *testing.T) {
	tempFile, err := os.CreateTemp("", "testdb")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	content := []byte(`{invalid json!!!`)
	if _, err := tempFile.Write(content); err != nil {
		t.Fatalf("Failed to write content to temporary file: %v", err)
	}
	tempFile.Close()

	_, err = loadChecksumDB(tempFile.Name(), false)
	if err == nil {
		t.Error("Expected an error for corrupt database file, got nil")
	}
}

func TestGetFilesToProcess(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	file1, err := os.CreateTemp(tempDir, "file1")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	file1.Close()

	file2, err := os.CreateTemp(tempDir, "file2")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	file2.Close()

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			file1.Name(): 1234,
		},
	}

	// Test case 1: "check" mode
	files, calculateChecksums, err := getFilesToProcess("check", []string{tempDir}, checksumDB)
	if err != nil {
		t.Errorf("Unexpected error in 'check' mode: %v", err)
	}
	expectedFiles := []string{file1.Name()}
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
	expectedFiles = []string{file1.Name()}
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
	expectedFiles = []string{file2.Name()}
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
	expectedFiles = []string{file2.Name()}
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
	expectedFiles = []string{file1.Name()}
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
	tempDir1, err := os.MkdirTemp("", "testdir1")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir1)

	tempDir2, err := os.MkdirTemp("", "testdir2")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir2)

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
	tempFile, err := os.CreateTemp("", "testdb")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			"file1": 1234,
			"file2": 5678,
		},
	}

	err = saveChecksumDB(tempFile.Name(), checksumDB, false)
	if err != nil {
		t.Fatalf("Failed to save checksum database: %v", err)
	}

	content, err := os.ReadFile(tempFile.Name())
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
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "sub", "checksums.json")
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}

	err = saveChecksumDB(dbPath, checksumDB, false)
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
	tempFile, err := os.CreateTemp("", "testfile")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())
	tempFile.Close()

	if !fileExists(tempFile.Name()) {
		t.Error("Expected fileExists to return true for an existing file")
	}

	if fileExists("nonexistent_file") {
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
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	file1, err := os.CreateTemp(tempDir, "file1")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	file1.Close()

	file2, err := os.CreateTemp(tempDir, "file2")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	file2.Close()

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			file1.Name(): 1234,
		},
	}

	files, err := walkDirectoriesForMissing([]string{tempDir}, checksumDB)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(files) != 1 || files[0] != file2.Name() {
		t.Errorf("Expected [%s], got %v", file2.Name(), files)
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
	_, err := calculateChecksum("/nonexistent/file/path")
	if err == nil {
		t.Error("Expected an error for nonexistent file")
	}
}

func TestLoadChecksumDBVerbose(t *testing.T) {
	// Test verbose with existing DB
	tempFile, err := os.CreateTemp("", "testdb")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	content := []byte(`{"checksums":{"file1":1234}}`)
	if _, err := tempFile.Write(content); err != nil {
		t.Fatalf("Failed to write: %v", err)
	}
	tempFile.Close()

	db, err := loadChecksumDB(tempFile.Name(), true)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(db.Checksums) != 1 {
		t.Errorf("Expected 1 entry, got %d", len(db.Checksums))
	}

	// Test verbose with missing DB
	db, err = loadChecksumDB("/nonexistent/path/db.json", true)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(db.Checksums) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(db.Checksums))
	}
}

func TestSaveChecksumDBVerbose(t *testing.T) {
	tempFile, err := os.CreateTemp("", "testdb")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}

	err = saveChecksumDB(tempFile.Name(), checksumDB, true)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
}

func TestSaveChecksumDBWriteError(t *testing.T) {
	// Use a path under /dev/null which can't have children
	err := saveChecksumDB("/dev/null/impossible/path/db.json", &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}, false)
	if err == nil {
		t.Error("Expected an error for impossible path")
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
	tempFile, err := os.CreateTemp("", "testdb")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())
	tempFile.Write([]byte(`{"checksums":{}}`))
	tempFile.Close()

	os.Chmod(tempFile.Name(), 0000)

	_, err = loadChecksumDB(tempFile.Name(), false)
	if err == nil {
		t.Error("Expected an error for unreadable database file")
	}
}

func TestSaveChecksumDBReadOnlyDir(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Make directory read-only so WriteFile fails
	os.Chmod(tempDir, 0555)

	dbPath := filepath.Join(tempDir, "checksums.json")
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{"file1": 1234},
	}
	err = saveChecksumDB(dbPath, checksumDB, false)
	if err == nil {
		t.Error("Expected an error writing to read-only directory")
	}
}

func TestGetFilesToProcessDeletedDirectoryFilter(t *testing.T) {
	absDir1, _ := filepath.Abs("/tmp/testdir_a")
	absDir2, _ := filepath.Abs("/tmp/testdir_b")
	file1Path := filepath.Join(absDir1, "file1.txt")
	file2Path := filepath.Join(absDir2, "file2.txt")

	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			file1Path: 1234,
			file2Path: 5678,
		},
	}

	// remove-deleted with directory filter
	files, calc, err := getFilesToProcess("remove-deleted", []string{"/tmp/testdir_a"}, checksumDB)
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
	files, _, err = getFilesToProcess("list-deleted", []string{"/tmp/testdir_b"}, checksumDB)
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
	_, err := walkDirectoriesForMissing([]string{"/nonexistent/directory"}, checksumDB)
	if err == nil {
		t.Error("Expected an error for nonexistent directory")
	}
}

func TestDefaultDBPath(t *testing.T) {
	path := defaultDBPath()
	if path == "" {
		t.Error("defaultDBPath returned empty string")
	}
	if !strings.Contains(path, "checksumtool") {
		t.Errorf("Expected path to contain 'checksumtool', got %s", path)
	}
	if !strings.HasSuffix(path, "checksums.json") {
		t.Errorf("Expected path to end with checksums.json, got %s", path)
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
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	tempFile, err := os.CreateTemp(tempDir, "testfile")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tempFile.Write([]byte("content"))
	tempFile.Close()

	jobs := make(chan string, 1)
	results := make(chan WorkerResult, 1)
	var wg sync.WaitGroup

	jobs <- tempFile.Name()
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
	cmd := exec.Command(binPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for missing -mode flag")
	}
	if !strings.Contains(string(out), "operation mode") {
		t.Errorf("Expected mode error message, got: %s", out)
	}

	// Test: invalid workers
	cmd = exec.Command(binPath, "-workers", "0", "-mode", "check")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for -workers 0")
	}
	if !strings.Contains(string(out), "workers") {
		t.Errorf("Expected workers error message, got: %s", out)
	}

	// Test: list-missing without directories
	cmd = exec.Command(binPath, "-mode", "list-missing")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for list-missing without dirs")
	}
	if !strings.Contains(string(out), "requires") {
		t.Errorf("Expected directory requirement message, got: %s", out)
	}

	// Test: add-missing without directories
	cmd = exec.Command(binPath, "-mode", "add-missing")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for add-missing without dirs")
	}

	// Test: check mode with no DB (should succeed, 0 files)
	dbPath := filepath.Join(t.TempDir(), "test.json")
	cmd = exec.Command(binPath, "-mode", "check", "-db", dbPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected success for check with empty DB, got: %v\n%s", err, out)
	}

	// Test: corrupt DB file
	corruptDB := filepath.Join(t.TempDir(), "corrupt.json")
	os.WriteFile(corruptDB, []byte("{bad json!"), 0600)
	cmd = exec.Command(binPath, "-mode", "check", "-db", corruptDB)
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
	os.WriteFile(testFile, []byte("hello"), 0644)
	freshDB := filepath.Join(t.TempDir(), "fresh.json")

	cmd = exec.Command(binPath, "-mode", "add-missing", "-db", freshDB, testDir)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected success for add-missing, got: %v\n%s", err, out)
	}

	// Verify DB was created
	if !fileExists(freshDB) {
		t.Fatal("Expected DB file to be created")
	}

	// Check should pass
	cmd = exec.Command(binPath, "-mode", "check", "-db", freshDB, testDir)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected check to pass, got: %v\n%s", err, out)
	}

	// Modify file, check should fail with exit 1
	os.WriteFile(testFile, []byte("modified"), 0644)
	cmd = exec.Command(binPath, "-mode", "check", "-db", freshDB, testDir)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Error("Expected non-zero exit for checksum mismatch")
	}
	if !strings.Contains(string(out), "Mismatch") {
		t.Errorf("Expected mismatch message, got: %s", out)
	}

	// Test: verbose mode
	cmd = exec.Command(binPath, "-mode", "check", "-db", freshDB, "-verbose", testDir)
	out, err = cmd.CombinedOutput()
	// Will exit 1 due to mismatch, but should have verbose output
	if !strings.Contains(string(out), "Loading checksum database") {
		t.Errorf("Expected verbose output, got: %s", out)
	}

	// Test: update mode with unreadable file should exit non-zero
	errDir := t.TempDir()
	errFile := filepath.Join(errDir, "unreadable.txt")
	os.WriteFile(errFile, []byte("data"), 0644)
	errDB := filepath.Join(t.TempDir(), "err.json")
	// First add the file
	cmd = exec.Command(binPath, "-mode", "add-missing", "-db", errDB, errDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to add-missing: %v\n%s", err, out)
	}
	// Make it unreadable, then update should fail
	os.Chmod(errFile, 0000)
	cmd = exec.Command(binPath, "-mode", "update", "-db", errDB, errDir)
	out, err = cmd.CombinedOutput()
	os.Chmod(errFile, 0644) // restore for cleanup
	if err == nil {
		t.Error("Expected non-zero exit for update mode with unreadable file")
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
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "checksums.json")
	checksumDB := &ChecksumDB{
		Checksums: map[string]uint64{
			"file1": 1234,
			"file2": 5678,
		},
	}

	err = saveChecksumDB(dbPath, checksumDB, false)
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
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "checksums.json")

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

func TestWalkDirectoriesForMissingSymlinkDir(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "testdir")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a real file
	realFile := filepath.Join(tempDir, "real.txt")
	os.WriteFile(realFile, []byte("data"), 0644)

	// Create a subdirectory with a file
	subDir := filepath.Join(tempDir, "subdir")
	os.Mkdir(subDir, 0755)
	subFile := filepath.Join(subDir, "sub.txt")
	os.WriteFile(subFile, []byte("sub"), 0644)

	// Create a symlink loop: tempDir/loop -> tempDir
	loopLink := filepath.Join(tempDir, "loop")
	os.Symlink(tempDir, loopLink)

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
}
