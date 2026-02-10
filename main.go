// checksumtool - A tool for calculating and comparing file checksums
// Copyright (C) 2024 Toni Melisma

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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

type ChecksumDB struct {
	Checksums map[string]uint64 `json:"checksums"`
	mutex     sync.Mutex
}

type WorkerResult struct {
	FilePath string
	Checksum uint64
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

	checksumDB := &ChecksumDB{Checksums: make(map[string]uint64)}
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
		return nil, fmt.Errorf("failed to parse database file: %w", err)
	}

	if verbose {
		fmt.Printf("Checksum database loaded with %d files\n", len(checksumDB.Checksums))
	}

	return checksumDB, nil
}

func saveChecksumDB(dbFilePath string, checksumDB *ChecksumDB, verbose bool) error {
	if verbose {
		fmt.Println("Saving checksum database...")
	}

	dbDir := filepath.Dir(dbFilePath)
	if err := os.MkdirAll(dbDir, 0700); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	checksumDB.mutex.Lock()
	dbData, err := json.MarshalIndent(checksumDB, "", "  ")
	checksumDB.mutex.Unlock()
	if err != nil {
		return fmt.Errorf("failed to marshal database: %w", err)
	}

	tmpFile, err := os.CreateTemp(dbDir, ".checksumtool-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(dbData); err != nil {
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

	if err := os.Rename(tmpPath, dbFilePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file to database: %w", err)
	}

	if verbose {
		fmt.Println("Checksum database saved.")
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
		err := filepath.WalkDir(directory, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() {
				return nil
			}

			// WalkDir doesn't follow symlinks into directories, so symlink
			// loops are not a risk. Process regular files and symlinked files
			// (workers follow symlinks when opening). Skip other types
			// (devices, pipes, sockets, etc.).
			if !d.Type().IsRegular() && d.Type()&os.ModeSymlink == 0 {
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

func getFilesToProcess(mode string, directories []string, checksumDB *ChecksumDB) ([]string, bool, error) {
	var filesToProcess []string
	var calculateChecksums bool

	switch mode {
	case "check", "update":
		calculateChecksums = true
		for filePath := range checksumDB.Checksums {
			absPath, err := filepath.Abs(filePath)
			if err != nil {
				return nil, false, fmt.Errorf("failed to get absolute path for %s: %v", filePath, err)
			}
			if len(directories) > 0 {
				for _, dir := range directories {
					absDir, err := filepath.Abs(dir)
					if err != nil {
						return nil, false, fmt.Errorf("failed to get absolute path for %s: %v", dir, err)
					}
					if isUnderDir(absPath, absDir) {
						filesToProcess = append(filesToProcess, absPath)
						break
					}
				}
			} else {
				filesToProcess = append(filesToProcess, absPath)
			}
		}
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

func worker(jobs <-chan string, results chan<- WorkerResult, calculateChecksums bool, wg *sync.WaitGroup) {
	defer wg.Done()

	for filePath := range jobs {
		result := WorkerResult{FilePath: filePath, Exists: true}

		if !fileExists(filePath) {
			result.Exists = false
			results <- result
			continue
		}

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

// processResults consumes worker results and applies mode-specific logic.
// Reads of checksumDB.Checksums without the mutex are safe here because
// processResults is the only goroutine that modifies the map after workers
// have started, and it runs sequentially per result.
func processResults(results <-chan WorkerResult, done chan<- struct{}, mode string, checksumDB *ChecksumDB, processedFiles *uint64, outputMu *sync.Mutex, prefix string) int {
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
			} else if storedChecksum, ok := checksumDB.Checksums[result.FilePath]; ok && result.Checksum != storedChecksum {
				outputMu.Lock()
				fmt.Printf("%sMismatch for file: %s\n", prefix, result.FilePath)
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
			if storedChecksum, ok := checksumDB.Checksums[result.FilePath]; !ok || result.Checksum != storedChecksum {
				outputMu.Lock()
				fmt.Printf("%sChanged or new file: %s\n", prefix, result.FilePath)
				outputMu.Unlock()
				checksumDB.mutex.Lock()
				checksumDB.Checksums[result.FilePath] = result.Checksum
				checksumDB.mutex.Unlock()
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
				checksumDB.Checksums[result.FilePath] = result.Checksum
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

func main() {
	var dbFilePath string
	var verbose bool
	var mode string
	var numWorkers int
	var configPath string
	flag.StringVar(&dbFilePath, "db", defaultDBPath(), "Checksum database file location")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose output")
	flag.StringVar(&mode, "mode", "", "Operation mode: check, update, list-missing, add-missing, remove-deleted, list-deleted")
	flag.IntVar(&numWorkers, "workers", 4, "Number of worker goroutines")
	flag.StringVar(&configPath, "config", defaultConfigPath(), "Config file location")
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

	if numWorkers <= 0 {
		fmt.Println("Error: -workers must be greater than 0")
		os.Exit(1)
	}

	if mode == "" {
		fmt.Println("Please specify an operation mode using the -mode flag: check, update, list-missing, add-missing, remove-deleted, list-deleted")
		os.Exit(1)
	}

	directories := flag.Args()

	// If no CLI directories, use config directories
	if len(directories) == 0 {
		directories = cfg.Directories
	}

	if (mode == "list-missing" || mode == "add-missing") && len(directories) == 0 {
		fmt.Printf("Error: %s mode requires at least one directory argument\n", mode)
		os.Exit(1)
	}

	// Acquire DB lock: exclusive for mutating modes, shared for read-only
	mutating := mode == "update" || mode == "add-missing" || mode == "remove-deleted"
	lockFile, err := lockDBFile(dbFilePath, mutating)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	defer unlockDBFile(lockFile)

	checksumDB, err := loadChecksumDB(dbFilePath, verbose)
	if err != nil {
		fmt.Printf("Error loading database: %v\n", err)
		os.Exit(1)
	}

	if verbose {
		fmt.Println("Comparing checksums with the database...")
	}

	filesToProcess, calculateChecksums, err := getFilesToProcess(mode, directories, checksumDB)
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
	go func() {
		mismatchCount = processResults(results, done, mode, checksumDB, &processedFiles, &outputMu, prefix)
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
