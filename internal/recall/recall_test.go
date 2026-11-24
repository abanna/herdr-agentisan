package recall_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/recall"
)

// write puts content at dir/name and returns the path.
func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func load(t *testing.T, src recall.Sources) recall.Library {
	t.Helper()
	lib, err := recall.Load(src)
	require.NoError(t, err)
	return lib
}

func ids(lib recall.Library) []string {
	out := make([]string, len(lib.Sections))
	for i, s := range lib.Sections {
		out[i] = s.ID
	}
	return out
}

const bulletFile = `# Standing facts

## 1. Machine

- Hello   world, the first rule.
  It goes on here,
  and on here.
- Second rule, one line.

- Third rule after a blank line,
  with a nested list:
  - nested item
## 2. Next

Loose paragraph, not a section.
- Fourth rule under another heading
 one space, so the section ends above this line.
`

// TestBulletFileCutsSectionsWithLineRanges: a section is one top-level bullet
// and its indented lines, word for word, with the 1-based lines it spans.
func TestBulletFileCutsSectionsWithLineRanges(t *testing.T) {
	t.Parallel()
	p := write(t, t.TempDir(), "STANDING.md", bulletFile)
	lib := load(t, recall.Sources{Bullets: []recall.Bullets{{Prefix: "std", Path: p}}})

	type cut struct {
		start, end int
		text       string
	}
	want := []cut{
		{5, 7, "- Hello   world, the first rule.\n  It goes on here,\n  and on here."},
		{8, 8, "- Second rule, one line."},
		{10, 12, "- Third rule after a blank line,\n  with a nested list:\n  - nested item"},
		{16, 16, "- Fourth rule under another heading"},
	}
	require.Len(t, lib.Sections, len(want))
	for i, w := range want {
		s := lib.Sections[i]
		assert.Equal(t, p, s.File)
		assert.Equal(t, w.start, s.Start, "section %d start", i)
		assert.Equal(t, w.end, s.End, "section %d end", i)
		assert.Equal(t, w.text, s.Text, "section %d is its lines, verbatim", i)
		assert.Regexp(t, `^std:[0-9a-f]{8}$`, s.ID)
	}
	assert.Equal(t, "Hello   world, the first rule.", lib.Sections[0].Heading, "the heading is the first line without its marker")
}

// TestBulletIDIsAHashOfTheFirstLine pins the ID's contract: the first 8 hex of
// the SHA-256 of the first line with its whitespace collapsed. A label made
// today names the same section after any release.
func TestBulletIDIsAHashOfTheFirstLine(t *testing.T) {
	t.Parallel()
	p := write(t, t.TempDir(), "r.md", "- Hello   world\n  tail\n")
	lib := load(t, recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: p}}})
	require.Len(t, lib.Sections, 1)
	assert.Equal(t, "rul:3a1d2b00", lib.Sections[0].ID, "sha256(\"- Hello world\")")
}

// TestBulletIDsSurviveTheEditsAroundThem: a section that stays keeps its ID
// when others are added, removed or edited below their first line, and a
// removed section is simply gone. Editing a first line is a new section.
func TestBulletIDsSurviveTheEditsAroundThem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := write(t, dir, "r.md", "- Alpha rule\n  more alpha\n- Beta rule\n- Gamma rule\n")
	src := recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: p}}}
	before := load(t, src)
	require.Len(t, before.Sections, 3)
	alpha, beta, gamma := before.Sections[0].ID, before.Sections[1].ID, before.Sections[2].ID
	assert.Len(t, map[string]bool{alpha: true, beta: true, gamma: true}, 3)

	// Prepend a new rule, edit alpha's second line, remove beta.
	write(t, dir, "r.md", "- New rule\n- Alpha rule\n  more alpha, edited\n- Gamma rule\n")
	after := load(t, src)
	assert.Equal(t, []string{after.Sections[0].ID, alpha, gamma}, ids(after))
	_, err := after.Pick([]string{beta})
	require.ErrorIs(t, err, recall.ErrUnknownID, "a removed section is gone")
	got, err := after.Pick([]string{alpha})
	require.NoError(t, err)
	assert.Equal(t, 2, got[0].Start, "its range moved with the file")
	assert.Equal(t, "- Alpha rule\n  more alpha, edited", got[0].Text)

	// A new first line is a new section.
	write(t, dir, "r.md", "- Alpha rule, reworded\n")
	assert.NotContains(t, ids(load(t, src)), alpha)
}

// TestRepeatedFirstLinesGetOccurrenceSuffixes: two bullets that open alike
// are told apart by order.
func TestRepeatedFirstLinesGetOccurrenceSuffixes(t *testing.T) {
	t.Parallel()
	p := write(t, t.TempDir(), "r.md", "- Same\n- Same\n- Same\n- Other\n")
	lib := load(t, recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: p}}})
	got := ids(lib)
	require.Len(t, got, 4)
	assert.Equal(t, got[0]+"-2", got[1])
	assert.Equal(t, got[0]+"-3", got[2])
	assert.Len(t, map[string]bool{got[0]: true, got[3]: true}, 2)
}

// TestCRLFLinesStayAsWritten: lines end at "\n", so a "\r" is part of the line
// and is printed back, never normalised.
func TestCRLFLinesStayAsWritten(t *testing.T) {
	t.Parallel()
	p := write(t, t.TempDir(), "r.md", "- One\r\n  two\r\n\r\n- Three\r\n")
	lib := load(t, recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: p}}})
	require.Len(t, lib.Sections, 2)
	assert.Equal(t, "- One\r\n  two\r", lib.Sections[0].Text)
	assert.Equal(t, "- Three\r", lib.Sections[1].Text)
}

const memoryFile = `---
name: boss-working-rules
description: "Standing rules: Agentisan always, only the user merges"
metadata:
  type: feedback
---

Body line one.

**Why:** because.
`

// TestMemoryFilesAreWholeSections: each *.md in a memory directory is one
// section, whole, named by its frontmatter; the index, hidden files, other
// types and subdirectories are not sections.
func TestMemoryFilesAreWholeSections(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := write(t, dir, "boss-working-rules.md", memoryFile)
	write(t, dir, "MEMORY.md", "- [x](x.md) — an index line\n")
	write(t, dir, ".hidden.md", "---\nname: hidden\n---\n")
	write(t, dir, "notes.txt", "not markdown")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub.md"), 0o750))
	write(t, filepath.Join(dir, "sub.md"), "inner.md", "inner")

	lib := load(t, recall.Sources{MemoryDirs: []string{dir}})
	require.Len(t, lib.Sections, 1)
	s := lib.Sections[0]
	assert.Equal(t, "mem:boss-working-rules", s.ID)
	assert.Equal(t, p, s.File)
	assert.Equal(t, 1, s.Start)
	assert.Equal(t, 10, s.End)
	assert.Equal(t, strings.TrimSuffix(memoryFile, "\n"), s.Text, "the whole file, word for word")
	assert.Equal(t, "Standing rules: Agentisan always, only the user merges", s.Heading, "the frontmatter's description, unquoted")
}

// TestMemoryIDs: the name comes from the frontmatter, else from the file's
// stem, cleaned to the ID alphabet; a repeated name is told apart by order of
// the files.
func TestMemoryIDs(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		files map[string]string // file name -> content
		want  []string
	}{
		"name beats the file name": {map[string]string{"a.md": "---\nname: other-name\n---\nx\n"}, []string{"mem:other-name"}},
		"single quotes":            {map[string]string{"a.md": "---\nname: 'quoted'\n---\nx\n"}, []string{"mem:quoted"}},
		"no frontmatter":           {map[string]string{"plain-note.md": "just text\n"}, []string{"mem:plain-note"}},
		"unclosed frontmatter":     {map[string]string{"open.md": "---\nname: lost\nbody\n"}, []string{"mem:open"}},
		"a name outside the alphabet is made one": {
			map[string]string{"My Note.md": "---\nname: Not A Slug!\n---\nx\n"}, []string{"mem:not-a-slug"},
		},
		"upper case is lowered":            {map[string]string{"a.md": "---\nname: Mixed-Case\n---\nx\n"}, []string{"mem:mixed-case"}},
		"a double-quoted name":             {map[string]string{"a.md": "---\nname: \"quoted-2\"\n---\nx\n"}, []string{"mem:quoted-2"}},
		"a file name outside it is too":    {map[string]string{"My Note.md": "plain\n"}, []string{"mem:my-note"}},
		"a name of nothing but marks":      {map[string]string{"!!!.md": "plain\n"}, []string{"mem:file"}},
		"an indented name is not the name": {map[string]string{"s.md": "---\nmetadata:\n  name: nested\n---\nx\n"}, []string{"mem:s"}},
		"a repeated name": {
			map[string]string{"a.md": "---\nname: same\n---\n1\n", "b.md": "---\nname: same\n---\n2\n"}, []string{"mem:same", "mem:same-2"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for f, c := range tc.files {
				write(t, dir, f, c)
			}
			assert.Equal(t, tc.want, ids(load(t, recall.Sources{MemoryDirs: []string{dir}})))
		})
	}
}

// TestHeadingFallsBackAndIsBounded: a memory file with no description is
// headed by its name; a long bullet line is cut to MaxHeading characters.
func TestHeadingFallsBackAndIsBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "n.md", "---\nname: no-description\n---\nbody\n")
	long := "- a" + strings.Repeat("é", recall.MaxHeading) + "\n"
	p := write(t, dir, "r.md", long)
	lib := load(t, recall.Sources{MemoryDirs: []string{dir}, Bullets: []recall.Bullets{{Prefix: "rul", Path: p}}})
	require.Len(t, lib.Sections, 3, "two memory files, one bullet")
	byID := map[string]recall.Section{}
	for _, s := range lib.Sections {
		byID[s.ID] = s
	}
	assert.Equal(t, "no-description", byID["mem:no-description"].Heading)
	for _, s := range lib.Sections {
		assert.LessOrEqual(t, utf8.RuneCountInString(s.Heading), recall.MaxHeading+1, s.ID)
		assert.True(t, utf8.ValidString(s.Heading), s.ID)
	}
	var cut recall.Section
	for _, s := range lib.Sections {
		if strings.HasPrefix(s.ID, "rul:") {
			cut = s
		}
	}
	assert.True(t, strings.HasSuffix(cut.Heading, "…"), "a cut heading says so")
}

// TestLibraryOrder: memory directories come first, then each bullet file, in
// the order given.
func TestLibraryOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "b.md", "---\nname: b\n---\n")
	write(t, dir, "a.md", "---\nname: a\n---\n")
	p1 := write(t, dir, "one.txt", "- one\n")
	p2 := write(t, dir, "two.txt", "- two\n")
	lib := load(t, recall.Sources{MemoryDirs: []string{dir}, Bullets: []recall.Bullets{{Prefix: "bbb", Path: p2}, {Prefix: "aaa", Path: p1}}})
	got := ids(lib)
	require.Len(t, got, 4)
	assert.Equal(t, []string{"mem:a", "mem:b"}, got[:2], "files in name order")
	assert.True(t, strings.HasPrefix(got[2], "bbb:") && strings.HasPrefix(got[3], "aaa:"), "bullet files as given")
}

// TestRef is the file:line a section prints under.
func TestRef(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/x/y.md:3-7", recall.Section{File: "/x/y.md", Start: 3, End: 7}.Ref())
	assert.Equal(t, "/x/y.md:3", recall.Section{File: "/x/y.md", Start: 3, End: 3}.Ref())
}

// TestLoadRefusesWhatItCannotRead: a library with a source missing would
// answer "nothing governs" for the rules in it, so any bad source fails the
// whole load, naming it.
func TestLoadRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ok := write(t, dir, "ok.md", "- fine\n")
	big := write(t, dir, "big.md", strings.Repeat("x", recall.MaxFile+1))
	notDir := write(t, dir, "plain", "x")
	// Paths that exist, but relative to this test's working directory.
	wd, err := os.Getwd()
	require.NoError(t, err)
	relFile, err := filepath.Rel(wd, ok)
	require.NoError(t, err)
	tests := map[string]struct {
		src    recall.Sources
		err    error
		namesY string
	}{
		"no source at all":            {recall.Sources{}, recall.ErrNoSources, ""},
		"a missing bullet file":       {recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: filepath.Join(dir, "gone.md")}}}, recall.ErrSource, "gone.md"},
		"a missing memory dir":        {recall.Sources{MemoryDirs: []string{filepath.Join(dir, "nodir")}}, recall.ErrSource, "nodir"},
		"a file as a memory dir":      {recall.Sources{MemoryDirs: []string{notDir}}, recall.ErrSource, "plain"},
		"a directory as a file":       {recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: dir}}}, recall.ErrSource, dir},
		"a file over MaxFile":         {recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: big}}}, recall.ErrSource, "big.md"},
		"a relative path":             {recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: "rel.md"}}}, recall.ErrSource, "rel.md"},
		"a relative file that exists": {recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: relFile}}}, recall.ErrSource, relFile},
		"a prefix of mem":             {recall.Sources{Bullets: []recall.Bullets{{Prefix: "mem", Path: ok}}}, recall.ErrSource, "mem"},
		"an empty prefix":             {recall.Sources{Bullets: []recall.Bullets{{Prefix: "", Path: ok}}}, recall.ErrSource, "prefix"},
		"an upper-case prefix":        {recall.Sources{Bullets: []recall.Bullets{{Prefix: "RUL", Path: ok}}}, recall.ErrSource, "RUL"},
		"a prefix used twice": {
			recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: ok}, {Prefix: "rul", Path: ok}}}, recall.ErrSource, "rul",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := recall.Load(tc.src)
			require.ErrorIs(t, err, tc.err)
			assert.Contains(t, err.Error(), tc.namesY)
		})
	}
}

// TestAnEmptySourceIsNoSections: a file or a directory that holds no section
// is a library of nothing from it, not a refusal.
func TestAnEmptySourceIsNoSections(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := write(t, dir, "e.md", "# only a heading\n")
	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.Mkdir(empty, 0o750))
	lib := load(t, recall.Sources{MemoryDirs: []string{empty}, Bullets: []recall.Bullets{{Prefix: "rul", Path: p}}})
	assert.Empty(t, lib.Sections)
}

// TestPickReadsSectionsInTheOrderAsked: a removed or edited section is named
// as unknown, every one of them, and nothing is returned for the rest, so a
// caller never acts on part of what it asked for.
func TestPickReadsSectionsInTheOrderAsked(t *testing.T) {
	t.Parallel()
	p := write(t, t.TempDir(), "r.md", "- One\n- Two\n- Three\n")
	lib := load(t, recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: p}}})
	a, b, c := lib.Sections[0].ID, lib.Sections[1].ID, lib.Sections[2].ID

	got, err := lib.Pick([]string{c, a, c})
	require.NoError(t, err)
	assert.Equal(t, []string{c, a, c}, ids(recall.Library{Sections: got}), "in the order asked, a repeat included")
	assert.Equal(t, "- Three", got[0].Text)

	got, err = lib.Pick([]string{a, "rul:00000000", b, "mem:gone"})
	require.ErrorIs(t, err, recall.ErrUnknownID)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "rul:00000000")
	assert.Contains(t, err.Error(), "mem:gone")
	assert.NotContains(t, err.Error(), b, "only the unknown ones are named")
}

// TestMemoryFrontmatterToleratesCRLFAndEmptyFiles: a --- block closed by a
// CRLF line still reads, and an empty file is no section.
func TestMemoryFrontmatterToleratesCRLFAndEmptyFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "a.md", "---\r\nname: crlf-name\r\ndescription: crlf description\r\n---\r\nbody\r\n")
	write(t, dir, "empty.md", "")
	lib := load(t, recall.Sources{MemoryDirs: []string{dir}})
	require.Len(t, lib.Sections, 1)
	assert.Equal(t, "mem:crlf-name", lib.Sections[0].ID)
	assert.Equal(t, "crlf description", lib.Sections[0].Heading)
	assert.Equal(t, 5, lib.Sections[0].End)
}
