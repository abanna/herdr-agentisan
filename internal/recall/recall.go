// Package recall is the boss's rule library (NERD-5741): the sections of the
// files its rules are kept in, read word for word at each call, never cached.
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
	"syscall"
	"unicode"
)

// Names and limits of the library.
const (
	MemoryPrefix = "mem"       // starts the ID of every memory file's section
	IndexFile    = "MEMORY.md" // a memory directory's index repeats the files' descriptions: no section
	MaxFile      = 4 << 20     // the most of one source file that is read
	MaxHeading   = 100         // the most characters of a heading, before its cut mark
	MaxName      = 60          // the most bytes of a memory ID after "mem:", so it is 64 at most
)

var (
	// ErrNoSources means no memory directory and no bullet file is named.
	ErrNoSources = errors.New("no recall sources: set recall.memory_dirs or recall.bullet_files in the shared herdr-agentisan.toml")
	// ErrSource means a source cannot be used: the whole load fails, or its rules would read as "nothing governs".
	ErrSource = errors.New("a recall source cannot be used")
	// ErrUnknownID means a section asked for is not in the library.
	ErrUnknownID = errors.New("no such section")
)

// Sources are the files the library is cut from; Deny is never read.
type Sources struct {
	MemoryDirs []string
	Bullets    []Bullets
	Deny       []string
}

// Bullets is a bullet file and the prefix its sections' IDs carry.
type Bullets struct{ Prefix, Path string }

// Section is one rule and where it is written.
type Section struct {
	// ID is mem:<name> (no two files share one), else <prefix>:<first 8 hex of
	// the SHA-256 of the bullet's text, whitespace collapsed>: those words or,
	// edited or removed, nothing. The same words again get -2, -3.
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
	file := map[string]string{} // ID -> the file it is in
	put := func(s Section) {
		file[s.ID] = s.File
		lib.Sections = append(lib.Sections, s)
	}
	addMemory := func(s Section) error {
		if other, ok := file[s.ID]; ok {
			return fmt.Errorf("%w: %s is the name of both %s and %s", ErrSource, s.ID, other, s.File)
		}
		put(s)
		return nil
	}
	addBullet := func(s Section) {
		base := s.ID
		for n := 2; file[s.ID] != ""; n++ {
			s.ID = base + "-" + strconv.Itoa(n)
		}
		put(s)
	}
	read := func(path string) ([]string, error) { return readLines(path, src.Deny) }
	for _, dir := range src.MemoryDirs {
		if err := loadMemoryDir(dir, read, addMemory); err != nil {
			return Library{}, err
		}
	}
	for _, b := range src.Bullets {
		lines, err := read(b.Path)
		if err != nil {
			return Library{}, err
		}
		for _, s := range cutBullets(b.Prefix, b.Path, lines) {
			addBullet(s)
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
		return nil, fmt.Errorf("%w: %s (removed, or edited? `herdr-agentisan recall list` shows what there is)", ErrUnknownID, strings.Join(unknown, ", "))
	}
	return out, nil
}

// readLines reads the file at path, at most MaxFile bytes, as the lines "\n" ends:
// never a denied file, a link or a non-regular file (a pipe is refused at once).
func readLines(path string, deny []string) ([]string, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: %s is not an absolute path", ErrSource, path)
	}
	if denied(path, deny) {
		return nil, fmt.Errorf("%w: %s is kept out of recall", ErrSource, path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- a source path the shared config names; only read.
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
	}
	text := strings.TrimPrefix(string(raw), "\uFEFF") // an editor's mark is not the first line's
	if text == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n"), nil
}

// denied reports whether path is, or is inside, one of deny, links resolved.
func denied(path string, deny []string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	p := resolve(path)
	for _, d := range deny {
		if r, err := filepath.Rel(resolve(d), p); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// loadMemoryDir adds each *.md of dir in name order, but the index and hidden files.
func loadMemoryDir(dir string, read func(string) ([]string, error), add func(Section) error) error {
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
		lines, err := read(path)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			continue
		}
		id, desc := frontmatter(lines)
		slug := slugRuns.ReplaceAllString(strings.ToLower(cmp.Or(id, strings.TrimSuffix(name, ".md"))), "-")
		id = strings.Trim(slug[:min(len(slug), MaxName)], "-")
		if err := add(Section{
			ID: MemoryPrefix + ":" + cmp.Or(id, "file"), File: path, Start: 1, End: len(lines),
			Heading: heading(cmp.Or(desc, id)), Text: strings.Join(lines, "\n"),
		}); err != nil {
			return err
		}
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

// cutBullets cuts each top-level "- " bullet of lines with the indented lines
// (two spaces or a tab) under it and the blanks between them, up to the last.
func cutBullets(prefix, path string, lines []string) []Section {
	blank := func(l string) bool { return strings.TrimSpace(l) == "" }
	var out []Section
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], "- ") {
			i++
			continue
		}
		end := i + 1 // past the last line of the section so far
		for j := i + 1; j < len(lines) && (blank(lines[j]) || strings.HasPrefix(lines[j], "  ") || strings.HasPrefix(lines[j], "\t")); j++ {
			if !blank(lines[j]) {
				end = j + 1
			}
		}
		text := strings.Join(lines[i:end], "\n")
		sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
		out = append(out, Section{
			ID: prefix + ":" + hex.EncodeToString(sum[:4]), File: path, Start: i + 1, End: end,
			Heading: heading(strings.TrimPrefix(lines[i], "- ")), Text: text,
		})
		i = end
	}
	return out
}

// heading is s trimmed, cut to MaxHeading characters, as valid UTF-8 with control characters escaped.
func heading(s string) string {
	r := []rune(strings.TrimSpace(s))
	cut := len(r) > MaxHeading
	var b strings.Builder
	for _, c := range r[:min(len(r), MaxHeading)] {
		if unicode.IsControl(c) {
			c := strconv.Quote(string(c))
			b.WriteString(c[1 : len(c)-1])
		} else {
			b.WriteRune(c)
		}
	}
	if cut {
		b.WriteString("…")
	}
	return b.String()
}
