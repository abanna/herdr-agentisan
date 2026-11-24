// Package recall is the boss's rule library (NERD-5741): the sections of the
// files its standing rules are kept in, each with an ID that stays the same
// while the section does, read word for word from those files at each call.
//
// A section is one memory file, whole, or one top-level bullet of a bullet
// file with its indented lines. Nothing here is cached, so a printed section
// is always what the file says now, and a section removed from its file is
// simply gone.
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
	"unicode/utf8"
)

const (
	// MemoryPrefix starts the ID of every memory file's section.
	MemoryPrefix = "mem"
	// IndexFile is a memory directory's index. Its lines repeat the files'
	// descriptions, so it is not a section.
	IndexFile = "MEMORY.md"
	// MaxFile is the most of one source file that is read.
	MaxFile = 4 << 20
	// MaxHeading is the most bytes of a section's heading, before the mark
	// that says it was cut.
	MaxHeading = 100
)

// Sentinel errors. Callers branch on them with errors.Is.
var (
	// ErrNoSources means no memory directory and no bullet file is named.
	ErrNoSources = errors.New("no recall sources: set recall.memory_dirs or recall.bullet_files in the shared herdr-agentisan.toml")
	// ErrSource means a source cannot be read, or is not a usable source.
	// A library without it would answer "nothing governs" for its rules, so
	// the whole load fails.
	ErrSource = errors.New("a recall source cannot be used")
	// ErrUnknownID means a section asked for is not in the library: removed,
	// or its first line edited.
	ErrUnknownID = errors.New("no such section")
)

// Sources are the files the library is cut from.
type Sources struct {
	// MemoryDirs are directories of memory files, each *.md a section.
	MemoryDirs []string
	// Bullets are files whose top-level bullets are sections.
	Bullets []Bullets
}

// Bullets is one bullet file and the prefix its sections' IDs carry.
type Bullets struct {
	Prefix string
	Path   string
}

// Section is one rule, with where it is written.
type Section struct {
	// ID is "<prefix>:<name or hash>": mem:<name> for a memory file,
	// <prefix>:<first 8 hex of the SHA-256 of the bullet's first line, its
	// whitespace collapsed> for a bullet, with -2, -3 for a repeat.
	ID string
	// File, Start and End are the file and the 1-based lines, both
	// inclusive, that Text is.
	File       string
	Start, End int
	// Heading is the memory file's description or the bullet's first line,
	// at most MaxHeading bytes.
	Heading string
	// Text is the lines word for word, without the last line's newline.
	Text string
}

// Ref is the section's place, file:line or file:first-last.
func (s Section) Ref() string {
	if s.Start == s.End {
		return s.File + ":" + strconv.Itoa(s.Start)
	}
	return s.File + ":" + strconv.Itoa(s.Start) + "-" + strconv.Itoa(s.End)
}

// Library is every section of the sources, memory directories first, then
// each bullet file as given, each in file order.
type Library struct {
	Sections []Section
}

var (
	prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}$`)
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	slugRuns      = regexp.MustCompile(`[^a-z0-9_-]+`)
)

// Load cuts the library from src, reading each file as it is now.
func Load(src Sources) (Library, error) {
	if len(src.MemoryDirs) == 0 && len(src.Bullets) == 0 {
		return Library{}, ErrNoSources
	}
	prefixes := map[string]bool{}
	for _, b := range src.Bullets {
		switch {
		case b.Prefix == MemoryPrefix:
			return Library{}, fmt.Errorf("%w: the prefix %q is the memory files'", ErrSource, b.Prefix)
		case !prefixPattern.MatchString(b.Prefix):
			return Library{}, fmt.Errorf("%w: the prefix %q is not a lower-case letter and up to 7 more letters or digits", ErrSource, b.Prefix)
		case prefixes[b.Prefix]:
			return Library{}, fmt.Errorf("%w: the prefix %q is used twice", ErrSource, b.Prefix)
		}
		prefixes[b.Prefix] = true
	}
	var lib Library
	seen := map[string]bool{}
	add := func(s Section) {
		s.ID = unique(seen, s.ID)
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

// Find is the section with id.
func (l Library) Find(id string) (Section, bool) {
	for _, s := range l.Sections {
		if s.ID == id {
			return s, true
		}
	}
	return Section{}, false
}

// Pick is the sections ids name, in the order asked. If any is not in the
// library, it is ErrUnknownID naming every one that is not, and no section.
func (l Library) Pick(ids []string) ([]Section, error) {
	out := make([]Section, 0, len(ids))
	var unknown []string
	for _, id := range ids {
		if s, ok := l.Find(id); ok {
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

// unique is id, or id-2, id-3 … when it is taken, and marks it taken.
func unique(seen map[string]bool, id string) string {
	out := id
	for n := 2; seen[out]; n++ {
		out = id + "-" + strconv.Itoa(n)
	}
	seen[out] = true
	return out
}

// readLines reads the file at path, at most MaxFile bytes of it, as the lines
// "\n" ends; the last line's newline is not a line of its own.
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
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSource, err)
	}
	if len(raw) > MaxFile {
		return nil, fmt.Errorf("%w: %s is over %d MiB", ErrSource, path, MaxFile>>20)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), nil
}

// loadMemoryDir adds one section for each *.md file in dir, in name order,
// but the index, a hidden file and a directory.
func loadMemoryDir(dir string, add func(Section)) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %s is not an absolute path", ErrSource, dir)
	}
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
		if !namePattern.MatchString(id) {
			id = slug(strings.TrimSuffix(name, ".md"))
		}
		add(Section{
			ID: MemoryPrefix + ":" + id, File: path, Start: 1, End: len(lines),
			Heading: heading(cmp.Or(desc, id)), Text: strings.Join(lines, "\n"),
		})
	}
	return nil
}

// frontmatter is the top-level name and description of a file that opens
// with a --- block; both are empty for a file whose block never closes.
func frontmatter(lines []string) (name, desc string) {
	if strings.TrimRight(lines[0], "\r") != "---" {
		return "", ""
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", ""
	}
	for _, l := range lines[1:end] {
		if v, ok := strings.CutPrefix(l, "name:"); ok {
			name = unquote(v)
		} else if v, ok := strings.CutPrefix(l, "description:"); ok {
			desc = unquote(v)
		}
	}
	return name, desc
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// slug is s as a name of the ID alphabet.
func slug(s string) string {
	s = strings.Trim(slugRuns.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "file"
	}
	return s
}

// cutBullets cuts each top-level "- " bullet of lines, with the lines under
// it that start with two spaces: a section ends at the first line that does
// not, a blank one included.
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

// heading is s trimmed, as valid UTF-8, and cut to MaxHeading bytes at a
// character boundary with a mark that says so.
func heading(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if len(s) <= MaxHeading {
		return s
	}
	i := MaxHeading
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}
