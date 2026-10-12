package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Verification times live beside the entries rather than inside them. An
// entry's value is what approvals and observation identities compare, and the
// time a file was last read says nothing about its content, so recording it
// must never make an entry look changed.

func (db *ChecksumDB) markVerified(path string, at int64) {
	if db.Verified == nil {
		db.Verified = make(map[string]int64)
	}
	db.Verified[path] = at
}

// pruneVerified drops verification times for paths no longer in the database,
// so moves and removals do not leave stale times behind.
func (db *ChecksumDB) pruneVerified() {
	for path := range db.Verified {
		if _, ok := db.Checksums[path]; !ok {
			delete(db.Verified, path)
		}
	}
	if len(db.Verified) == 0 {
		db.Verified = nil
	}
}

// parseByteSize accepts a plain byte count or a number with a K, M, G or T
// suffix (1024-based, an optional trailing "B" or "iB" is ignored).
func parseByteSize(text string) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(text))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "IB"), "B")
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	multiplier := int64(1)
	switch s[len(s)-1] {
	case 'K':
		multiplier = 1 << 10
	case 'M':
		multiplier = 1 << 20
	case 'G':
		multiplier = 1 << 30
	case 'T':
		multiplier = 1 << 40
	}
	if multiplier != 1 {
		s = s[:len(s)-1]
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid size %q", text)
	}
	return int64(value * float64(multiplier)), nil
}

// quickSelect narrows a sync's database entries to the ones a quick run must
// read: entries whose file is gone or whose size or timestamp changed, entries
// with no recorded metadata, and -- up to budget bytes -- the unchanged entries
// verified longest ago. Unchanged entries outside the budget are not read; bit
// rot keeps size and timestamp, so only the budgeted re-reads can find it.
func quickSelect(paths []string, db *ChecksumDB, budget int64) (selected []string, unread int) {
	type candidate struct {
		path     string
		size     int64
		verified int64
	}
	var unchanged []candidate
	for _, path := range paths {
		entry := db.Checksums[path]
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || !entry.MetadataKnown() ||
			!entry.MetadataMatches(info.Size(), info.ModTime().UnixNano()) {
			selected = append(selected, path)
			continue
		}
		unchanged = append(unchanged, candidate{path: path, size: entry.Size, verified: db.Verified[path]})
	}
	sort.Slice(unchanged, func(i, j int) bool {
		if unchanged[i].verified != unchanged[j].verified {
			return unchanged[i].verified < unchanged[j].verified
		}
		return unchanged[i].path < unchanged[j].path
	})
	var spent int64
	for _, c := range unchanged {
		if spent >= budget {
			unread++
			continue
		}
		selected = append(selected, c.path)
		spent += c.size
	}
	return selected, unread
}

var applyCategoryNames = map[string]bool{
	"metadata": true, "modified": true, "moved": true, "new": true,
	"corrupt": true, "unverifiable": true, "deleted": true,
}

func parseApplyCategories(text string) (map[string]bool, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	categories := make(map[string]bool)
	for _, name := range strings.Split(text, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !applyCategoryNames[name] {
			return nil, fmt.Errorf("unknown category %q (use metadata, modified, moved, new, corrupt, unverifiable or deleted)", name)
		}
		categories[name] = true
	}
	if len(categories) == 0 {
		return nil, fmt.Errorf("-apply-categories names no category")
	}
	return categories, nil
}

var listCategoryNames = map[string]bool{
	"metadata": true, "modified": true, "moved": true, "move_source": true, "new": true,
	"corrupt": true, "unverifiable": true, "deleted": true,
}

// parseListPaths reads "category=file[,category=file...]".
func parseListPaths(text string) (map[string]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	lists := make(map[string]string)
	for _, part := range strings.Split(text, ",") {
		name, file, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || file == "" {
			return nil, fmt.Errorf("invalid -list-paths item %q (want category=file)", part)
		}
		if !listCategoryNames[name] {
			return nil, fmt.Errorf("unknown -list-paths category %q (use new, moved, move_source, modified, corrupt, unverifiable, deleted or metadata)", name)
		}
		lists[name] = file
	}
	return lists, nil
}

func reportPaths(report *ScanReport, category string) []string {
	var paths []string
	files := func(list []ScanFile) {
		for _, f := range list {
			paths = append(paths, f.Path)
		}
	}
	switch category {
	case "new":
		files(report.New)
	case "modified":
		files(report.Modified)
	case "corrupt":
		files(report.Corrupt)
	case "unverifiable":
		files(report.Unverifiable)
	case "metadata":
		files(report.Touched)
	case "deleted":
		paths = append(paths, report.Deleted...)
	case "moved":
		for _, m := range report.Moved {
			paths = append(paths, m.To)
		}
	case "move_source":
		for _, m := range report.Moved {
			paths = append(paths, m.From)
		}
	}
	sort.Strings(paths)
	return paths
}

// writePathLists writes one path per line for each requested category. A name
// containing a newline cannot be represented in that format, so it is left out
// and reported as an error rather than silently mangled.
func writePathLists(report *ScanReport, lists map[string]string, relativeTo string) []error {
	var errs []error
	base := ""
	if relativeTo != "" {
		abs, err := filepath.Abs(relativeTo)
		if err != nil {
			return []error{fmt.Errorf("-relative-to: %v", err)}
		}
		base = abs
	}
	names := make([]string, 0, len(lists))
	for name := range lists {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var b strings.Builder
		for _, path := range reportPaths(report, name) {
			if strings.ContainsAny(path, "\n\r") {
				errs = append(errs, fmt.Errorf("%s path contains a line break and was not listed: %q", name, path))
				continue
			}
			out := path
			if base != "" {
				if !isUnderDir(path, base) {
					errs = append(errs, fmt.Errorf("%s path is outside -relative-to %s and was not listed: %s", name, base, path))
					continue
				}
				rel, err := filepath.Rel(base, path)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s path %s: %v", name, path, err))
					continue
				}
				out = rel
			}
			b.WriteString(out)
			b.WriteByte('\n')
		}
		if err := writeFileAtomic(lists[name], []byte(b.String())); err != nil {
			errs = append(errs, fmt.Errorf("writing %s list: %v", name, err))
		}
	}
	return errs
}

// printStatus summarizes the database: how much is recorded and how recently
// it was confirmed by actually reading the files.
func printStatus(db *ChecksumDB, scanReportPath string, now time.Time) {
	var ages []int64
	never := 0
	var bytes int64
	for path, entry := range db.Checksums {
		bytes += entry.Size
		if at, ok := db.Verified[path]; ok && at > 0 {
			ages = append(ages, now.Unix()-at)
		} else {
			never++
		}
	}
	fmt.Printf("Entries:            %d (%s recorded)\n", len(db.Checksums), formatBytes(bytes))
	fmt.Printf("Never re-verified:  %d (no verification time recorded)\n", never)
	if len(ages) > 0 {
		sort.Slice(ages, func(i, j int) bool { return ages[i] < ages[j] })
		day := float64(24 * 60 * 60)
		fmt.Printf("Oldest verification: %.1f days ago\n", float64(ages[len(ages)-1])/day)
		fmt.Printf("Median verification: %.1f days ago\n", float64(ages[len(ages)/2])/day)
	}
	if report, err := loadScanReport(scanReportPath); err == nil {
		fmt.Printf("Unapplied scan report from %s: %d modified, %d corrupt, %d unverifiable, %d deleted, %d moved, %d new\n",
			time.Unix(report.GeneratedAt, 0).Format(time.RFC3339), len(report.Modified), len(report.Corrupt),
			len(report.Unverifiable), len(report.Deleted), len(report.Moved), len(report.New))
	}
}

func formatBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
