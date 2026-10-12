package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{"0": 0, "512": 512, "1K": 1024, "2M": 2 << 20, "115G": 115 << 30, "1.5G": 3 << 29, "1TiB": 1 << 40, "3gb": 3 << 30}
	for text, want := range cases {
		got, err := parseByteSize(text)
		if err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "-1G", "G"} {
		if _, err := parseByteSize(bad); err == nil {
			t.Errorf("parseByteSize(%q) accepted invalid input", bad)
		}
	}
}

func TestQuickSelectRotatesByVerificationAge(t *testing.T) {
	dir := t.TempDir()
	db := &ChecksumDB{Checksums: map[string]FileEntry{}}
	var paths []string
	for _, name := range []string{"a", "b", "c"} {
		path := filepath.Join(dir, name)
		writeTestFile(t, path, []byte("0123456789"))
		info, _ := os.Stat(path)
		db.Checksums[path] = FileEntry{Checksum: 1, Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		paths = append(paths, path)
	}
	db.markVerified(paths[0], 300)
	db.markVerified(paths[1], 100)
	// c has no time: never verified, so it is oldest.

	selected, unread := quickSelect(paths, db, 10)
	if len(selected) != 1 || selected[0] != paths[2] || unread != 2 {
		t.Fatalf("first selection = %v (unread %d); want only the never-verified file", selected, unread)
	}
	db.markVerified(paths[2], 400)
	selected, _ = quickSelect(paths, db, 10)
	if len(selected) != 1 || selected[0] != paths[1] {
		t.Fatalf("second selection = %v; want the oldest verified file", selected)
	}
	selected, unread = quickSelect(paths, db, 0)
	if len(selected) != 0 || unread != 3 {
		t.Fatalf("zero budget selected %v (unread %d); want nothing read", selected, unread)
	}

	// A changed or missing file is always read, whatever the budget.
	writeTestFile(t, paths[0], []byte("changed content"))
	os.Remove(paths[1])
	selected, _ = quickSelect(paths, db, 0)
	sort.Strings(selected)
	if len(selected) != 2 || selected[0] != paths[0] || selected[1] != paths[1] {
		t.Fatalf("changed/missing selection = %v", selected)
	}
}

func TestVerifiedTimesDoNotChangeEntries(t *testing.T) {
	db := &ChecksumDB{Checksums: map[string]FileEntry{"/x/a": {Checksum: 1, Size: 2, ModTime: 3}}}
	before := db.Checksums["/x/a"]
	db.markVerified("/x/a", 42)
	db.markVerified("/x/gone", 42)
	if db.Checksums["/x/a"] != before {
		t.Fatal("recording a verification time changed the entry")
	}
	db.pruneVerified()
	if _, ok := db.Verified["/x/gone"]; ok || db.Verified["/x/a"] != 42 {
		t.Fatalf("prune kept stale or dropped live times: %v", db.Verified)
	}
}

func TestOlderReadersIgnoreVerifiedTimes(t *testing.T) {
	data, err := json.Marshal(&ChecksumDB{
		Checksums: map[string]FileEntry{"/x/a": {Checksum: 1, Size: 2, ModTime: 3}},
		Verified:  map[string]int64{"/x/a": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The v3.3 database shape: only "checksums".
	var older struct {
		Checksums map[string]FileEntry `json:"checksums"`
	}
	if err := json.Unmarshal(data, &older); err != nil || older.Checksums["/x/a"].Checksum != 1 {
		t.Fatalf("v3.3-shaped reader failed: %v %v", err, older)
	}
}

func TestWritePathListsRefusesLineBreaks(t *testing.T) {
	dir := t.TempDir()
	report := &ScanReport{New: []ScanFile{{Path: filepath.Join(dir, "ok #1.txt")}, {Path: filepath.Join(dir, "bad\nname")}, {Path: "/elsewhere/x"}}}
	out := filepath.Join(dir, "new.list")
	errs := writePathLists(report, map[string]string{"new": out}, dir)
	if len(errs) != 2 {
		t.Fatalf("errors = %v; want the line-break and outside-base paths reported", errs)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "ok #1.txt\n" {
		t.Fatalf("list = %q", data)
	}
}

func TestApplyCategoriesValidation(t *testing.T) {
	if _, err := parseApplyCategories("new,bogus"); err == nil {
		t.Fatal("unknown category accepted")
	}
	got, err := parseApplyCategories(" new , moved ")
	if err != nil || !got["new"] || !got["moved"] || len(got) != 2 {
		t.Fatalf("parse = %v, %v", got, err)
	}
}

func readList(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestQuickSyncEndToEnd drives the binary through the unattended use the
// features exist for: record additions and moves, leave every content change
// and deletion for review, hand the paths to another tool, and let a later
// budgeted run find bit rot that a quick run cannot see.
func TestQuickSyncEndToEnd(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "checksumtool")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	work := t.TempDir()
	root := filepath.Join(work, "root")
	dbPath := filepath.Join(work, "db.json")
	lists := filepath.Join(work, "lists")
	os.MkdirAll(lists, 0o755)
	for name, content := range map[string]string{"keep.txt": "keep", "rot.txt": "rot-original", "edit.txt": "edit", "gone.txt": "gone", "move.txt": "move me"} {
		writeTestFile(t, filepath.Join(root, name), []byte(content))
	}
	run := func(args ...string) (string, int) {
		cmd := hermeticCommand(t, binPath, append([]string{"-db", dbPath}, args...)...)
		out, err := cmd.CombinedOutput()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}
	if out, code := run("-mode", "add-missing", root); code != 0 {
		t.Fatalf("add-missing: %d %s", code, out)
	}

	// Bit rot: same size, same timestamp, different bytes.
	rot := filepath.Join(root, "rot.txt")
	info, _ := os.Stat(rot)
	writeTestFile(t, rot, []byte("rot-0riginal"))
	os.Chtimes(rot, info.ModTime(), info.ModTime())
	// An ordinary edit, a deletion, a move and an addition.
	time.Sleep(10 * time.Millisecond)
	writeTestFile(t, filepath.Join(root, "edit.txt"), []byte("edited content"))
	os.Remove(filepath.Join(root, "gone.txt"))
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.Rename(filepath.Join(root, "move.txt"), filepath.Join(root, "sub", "move.txt"))
	writeTestFile(t, filepath.Join(root, "new.txt"), []byte("brand new"))

	listArg := "new=" + filepath.Join(lists, "new") + ",moved=" + filepath.Join(lists, "moved") +
		",move_source=" + filepath.Join(lists, "src") + ",modified=" + filepath.Join(lists, "mod") +
		",deleted=" + filepath.Join(lists, "del") + ",corrupt=" + filepath.Join(lists, "corrupt")
	out, code := run("-mode", "sync", "-quick", "-verify-budget", "0", "-apply", "-apply-categories", "new,moved,metadata",
		"-list-paths", listArg, "-relative-to", root, "-manifest", filepath.Join(work, "m.json"), root)
	if code != 1 {
		t.Fatalf("quick sync exit %d; want 1 for the pending edit and deletion\n%s", code, out)
	}
	expect := map[string][]string{"new": {"new.txt"}, "moved": {"sub/move.txt"}, "src": {"move.txt"}, "mod": {"edit.txt"}, "del": {"gone.txt"}, "corrupt": nil}
	for name, want := range expect {
		if got := readList(t, filepath.Join(lists, name)); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s list = %v; want %v", name, got, want)
		}
	}

	db, err := loadChecksumDB(dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	abs := func(name string) string { return filepath.Join(root, name) }
	if _, ok := db.Checksums[abs("new.txt")]; !ok {
		t.Error("new file was not recorded")
	}
	if _, ok := db.Checksums[abs("sub/move.txt")]; !ok {
		t.Error("move destination was not recorded")
	}
	if _, ok := db.Checksums[abs("move.txt")]; ok {
		t.Error("move source was not removed")
	}
	if _, ok := db.Checksums[abs("gone.txt")]; !ok {
		t.Error("deletion was applied without being allowed")
	}
	if db.Verified[abs("new.txt")] == 0 {
		t.Error("new file has no verification time")
	}
	before := db.Checksums[abs("edit.txt")]
	if got := calculateChecksumOrZero(abs("edit.txt")); got == before.Checksum {
		t.Fatal("test setup: edit did not change content")
	}

	// A second quick run with a budget large enough to re-read everything
	// unchanged finds the rot, and still applies nothing it was not allowed to.
	out, code = run("-mode", "sync", "-quick", "-verify-budget", "1T", "-apply", "-apply-categories", "new,moved,metadata",
		"-list-paths", "corrupt="+filepath.Join(lists, "corrupt"), root)
	if code != 1 || !strings.Contains(out, "rot.txt") {
		t.Fatalf("budgeted run did not report the rot (exit %d)\n%s", code, out)
	}
	if got := readList(t, filepath.Join(lists, "corrupt")); len(got) != 1 || got[0] != abs("rot.txt") {
		t.Errorf("corrupt list = %v", got)
	}
	db, _ = loadChecksumDB(dbPath, false)
	if db.Checksums[abs("edit.txt")] != before {
		t.Error("the pending edit was applied")
	}

	out, code = run("-mode", "status")
	if code != 0 || !strings.Contains(out, "Entries:") || !strings.Contains(out, "Oldest verification") {
		t.Fatalf("status exit %d\n%s", code, out)
	}

	if _, code = run("-mode", "check", "-verify-budget", "1G"); code == 0 {
		t.Error("-verify-budget accepted outside sync -quick")
	}
}

func calculateChecksumOrZero(path string) uint64 {
	sum, err := calculateChecksum(path)
	if err != nil {
		return 0
	}
	return sum
}
