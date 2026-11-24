package settings

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/abanna/herdr-agentisan/internal/recall"
)

// [recall] is where the boss's rules are kept (NERD-5741): memory directories,
// each *.md a section, and bullet files, each top-level bullet a section, under
// the prefix its sections' IDs carry. Like [debugger] it belongs to the shared
// file and --set only: the paths are this machine's, and a committed file must
// not name what a command reads.
const (
	recallMemoryDirsPath  = "recall.memory_dirs"
	recallBulletFilesPath = "recall.bullet_files"
)

// MaxRecallSources bounds the directories recall.memory_dirs may name.
const MaxRecallSources = 8

// recallPrefixPattern is a bullet file's prefix: the start of its sections' IDs.
var recallPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}$`)

var recallSchema = &node{kind: kindTable, fields: map[string]*node{
	"memory_dirs":  leafOf(kindStrings),
	"bullet_files": {kind: kindNames, elem: leafOf(kindString)},
}}

// checkRecall checks every path is absolute or under ~/, which is expanded
// with HOME when it is read, that no directory or file is named twice, and
// each bullet file's prefix.
func (m merged) checkRecall() error {
	v, _ := m.get(recallMemoryDirsPath)
	dirs, _ := v.([]string)
	if len(dirs) > MaxRecallSources {
		return m.invalidf(recallMemoryDirsPath, "%d directories; at most %d", len(dirs), MaxRecallSources)
	}
	var seen []string
	for i, d := range dirs {
		if !validEnvFile(d) {
			return m.invalidf(recallMemoryDirsPath, "entry %d, %s, is %s", i+1, FormatValue(d), envFileHint)
		}
		if slices.Contains(seen, filepath.Clean(d)) {
			return m.invalidf(recallMemoryDirsPath, "entry %d, %s, names a directory twice", i+1, FormatValue(d))
		}
		seen = append(seen, filepath.Clean(d))
	}
	v, _ = m.get(recallBulletFilesPath)
	files, _ := v.(map[string]any)
	seen = nil
	for _, prefix := range slices.Sorted(maps.Keys(files)) {
		path := recallBulletFilesPath + "." + prefix
		p, _ := files[prefix].(string)
		switch {
		case prefix == recall.MemoryPrefix:
			return m.invalidf(path, "%q is the prefix of the memory files' sections", prefix)
		case !recallPrefixPattern.MatchString(prefix):
			return m.invalidf(path, "a prefix is a lower-case letter and up to 7 more lower-case letters or digits")
		case !validEnvFile(p):
			return m.invalidf(path, "%s is %s", FormatValue(p), envFileHint)
		case slices.Contains(seen, filepath.Clean(p)):
			return m.invalidf(recallBulletFilesPath, "%s names a file twice", FormatValue(p))
		}
		seen = append(seen, filepath.Clean(p))
	}
	return nil
}

// RecallSources is [recall] from the shared file in configDir, each path with
// a leading ~/ expanded through lookupEnv's HOME, the bullet files by prefix.
// No configDir, and no file or table in it, is no sources.
func RecallSources(configDir string, lookupEnv func(string) (string, bool)) (recall.Sources, error) {
	if configDir == "" {
		return recall.Sources{}, nil
	}
	res, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return recall.Sources{}, err
	}
	var out recall.Sources
	v, _ := lookup(res.Values, recallMemoryDirsPath)
	dirs, _ := v.([]string)
	for _, d := range dirs {
		p, err := ExpandHome(d, lookupEnv)
		if err != nil {
			return recall.Sources{}, err
		}
		out.MemoryDirs = append(out.MemoryDirs, p)
	}
	v, _ = lookup(res.Values, recallBulletFilesPath)
	files, _ := v.(map[string]any)
	for _, prefix := range slices.Sorted(maps.Keys(files)) {
		s, _ := files[prefix].(string)
		p, err := ExpandHome(s, lookupEnv)
		if err != nil {
			return recall.Sources{}, err
		}
		out.Bullets = append(out.Bullets, recall.Bullets{Prefix: prefix, Path: p})
	}
	return out, nil
}
