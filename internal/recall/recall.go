// Package recall is the boss's rule library (NERD-5741): the sections of the
// files its standing rules are kept in, each with an ID that stays while the
// section does, read word for word at each call. Nothing is cached.
package recall

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// What the library is cut by.
const (
	MemoryPrefix = "mem"       // starts the ID of every memory file's section
	IndexFile    = "MEMORY.md" // a memory directory's index repeats the files' descriptions: no section
	MaxFile      = 4 << 20     // the most of one source file that is read
	MaxHeading   = 100         // the most characters of a heading, before its cut mark
)

var (
	// ErrNoSources means no memory directory and no bullet file is named.
	ErrNoSources = errors.New("no recall sources: set recall.memory_dirs or recall.bullet_files in the shared herdr-agentisan.toml")
	// ErrSource means a source cannot be used: the whole load fails, or its rules would read as "nothing governs".
	ErrSource = errors.New("a recall source cannot be used")
	// ErrUnknownID means a section asked for is not in the library.
	ErrUnknownID = errors.New("no such section")
)

// Sources are the files the library is cut from: directories of memory files,
// each *.md a section, and files whose top-level bullets are sections.
type Sources struct {
	MemoryDirs []string
	Bullets    []Bullets
}

// Bullets is a bullet file and the prefix its sections' IDs carry.
type Bullets struct{ Prefix, Path string }

// Section is one rule and where it is written.
type Section struct {
	// ID is mem:<name> for a memory file, else <prefix>:<first 8 hex of the
	// SHA-256 of the bullet's first line, whitespace collapsed>; a repeat gets
	// -2, -3.
	ID string
	// File, Start and End are where Text is: 1-based lines, both inclusive.
	File       string
	Start, End int
	// Heading is the memory file's description or the bullet's first line.
	Heading string
	// Text is those lines word for word, without the last one's newline.
	Text string
}

// Ref is the section's place, file:line or file:first-last.
func (s Section) Ref() string {
	if s.Start == s.End {
		return s.File + ":" + strconv.Itoa(s.Start)
	}
	return s.File + ":" + strconv.Itoa(s.Start) + "-" + strconv.Itoa(s.End)
}

// Library is the sections: memory directories first, then each bullet file as given.
type Library struct{ Sections []Section }

var (
	prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}$`)
	slugRuns      = regexp.MustCompile(`[^a-z0-9_-]+`)
)

// Load cuts the library from src, reading each file as it is now.
func Load(src Sources) (Library, error) {
	if len(src.MemoryDirs) == 0 && len(src.Bullets) == 0 {
		return Library{}, ErrNoSources
	}
	used := map[string]bool{MemoryPrefix: true}
	for _, b := range src.Bullets {
		if used[b.Prefix] || !prefixPattern.MatchString(b.Prefix) {
			return Library{}, fmt.Errorf("%w: the prefix %q is not one of a lower-case letter and up to 7 more letters or digits, other than mem, used once", ErrSource, b.Prefix)
		}
		used[b.Prefix] = true
	}
	var lib Library
	seen := map[string]bool{}
	add := func(s Section) {
		base := s.ID
		for n := 2; seen[s.ID]; n++ {
			s.ID = base + "-" + strconv.Itoa(n)
		}
		seen[s.ID] = true
		lib.Sections = append(lib.Sections, s)
	}
	for _, dir := range src.MemoryDirs {
		if err := loadMemoryDir(dir, add); err != nil {
			return Library{}, err
		}
	}
	for _, b := range src.Bullets {
		lines, err := readLines(b.Path)
		if err != nil {
			return Library{}, err
		}
		for _, s := range cutBullets(b.Prefix, b.Path, lines) {
			add(s)
		}
	}
	return lib, nil
}

// Pick is the sections ids name, in order; if any is gone, ErrUnknownID names them all.
func (l Library) Pick(ids []string) ([]Section, error) {
	byID := make(map[string]Section, len(l.Sections))
	for _, s := range l.Sections {
		byID[s.ID] = s
	}
	var out []Section
	var unknown []string
	for _, id := range ids {
		if s, ok := byID[id]; ok {
			out = append(out, s)
		} else {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("%w: %s (removed, or its first line edited? `herdr-agentisan recall list` shows what there is)", ErrUnknownID, strings.Join(unknown, ", "))
	}
	return out, nil
}

// readLines reads the file at path, at most MaxFile bytes, as the lines "\n" ends.
func readLines(path string) ([]string, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: %s is not an absolute path", ErrSource, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSource, err)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrSource, path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrSource, err)
	case len(raw) > MaxFile:
		return nil, fmt.Errorf("%w: %s is over %d MiB", ErrSource, path, MaxFile>>20)
	case len(raw) == 0:
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), nil
}

// loadMemoryDir adds a section for each *.md in dir, in name order, but the
// index, a hidden file and a directory.
func loadMemoryDir(dir string, add func(Section)) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSource, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") || name == IndexFile {
			continue
		}
		path := filepath.Join(dir, name)
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			continue
		}
		id, desc := frontmatter(lines)
		id = strings.Trim(slugRuns.ReplaceAllString(strings.ToLower(cmp.Or(id, strings.TrimSuffix(name, ".md"))), "-"), "-")
		add(Section{
			ID: MemoryPrefix + ":" + cmp.Or(id, "file"), File: path, Start: 1, End: len(lines),
			Heading: heading(cmp.Or(desc, id)), Text: strings.Join(lines, "\n"),
		})
	}
	return nil
}

// frontmatter is the top-level name and description of a closed --- block.
func frontmatter(lines []string) (name, desc string) {
	if strings.TrimRight(lines[0], "\r") != "---" {
		return "", ""
	}
	for i, l := range lines[1:] {
		if strings.TrimRight(l, "\r") == "---" {
			for _, l := range lines[1 : i+1] {
				if v, ok := strings.CutPrefix(l, "name:"); ok {
					name = strings.Trim(strings.TrimSpace(v), `"'`)
				} else if v, ok := strings.CutPrefix(l, "description:"); ok {
					desc = strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
			return name, desc
		}
	}
	return "", ""
}

// cutBullets cuts each top-level "- " bullet of lines with the lines under it
// that start with two spaces; a section ends at the first line that does not,
// a blank one included.
func cutBullets(prefix, path string, lines []string) []Section {
	var out []Section
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], "- ") {
			i++
			continue
		}
		j := i + 1
		for j < len(lines) && strings.HasPrefix(lines[j], "  ") {
			j++
		}
		sum := sha256.Sum256([]byte(strings.Join(strings.Fields(lines[i]), " ")))
		out = append(out, Section{
			ID: prefix + ":" + hex.EncodeToString(sum[:4]), File: path, Start: i + 1, End: j,
			Heading: heading(strings.TrimPrefix(lines[i], "- ")), Text: strings.Join(lines[i:j], "\n"),
		})
		i = j
	}
	return out
}

// heading is s trimmed and cut to MaxHeading characters, with a mark that says
// so. Converting to runes also makes it valid UTF-8.
func heading(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > MaxHeading {
		return string(r[:MaxHeading]) + "…"
	}
	return string(r)
}
