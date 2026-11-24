package settings

import (
	"maps"
	"slices"

	"github.com/abanna/herdr-agentisan/internal/recall"
)

// [recall] is where the boss's rules are kept (NERD-5741): memory directories,
// and bullet files each under the prefix its sections' IDs carry. Like
// [debugger] it belongs to the shared file and --set only: a committed file
// must not name what a command reads.
const (
	recallMemoryDirsPath  = "recall.memory_dirs"
	recallBulletFilesPath = "recall.bullet_files"
)

var recallSchema = &node{kind: kindTable, fields: map[string]*node{
	"memory_dirs":  leafOf(kindStrings),
	"bullet_files": {kind: kindNames, elem: leafOf(kindString)},
}}

// checkRecall checks every path is absolute or under ~/, which is expanded with
// HOME when it is read.
func (m merged) checkRecall() error {
	v, _ := m.get(recallMemoryDirsPath)
	dirs, _ := v.([]string)
	for i, d := range dirs {
		if !validEnvFile(d) {
			return m.invalidf(recallMemoryDirsPath, "entry %d, %s, is %s", i+1, FormatValue(d), envFileHint)
		}
	}
	v, _ = m.get(recallBulletFilesPath)
	files, _ := v.(map[string]any)
	for _, prefix := range slices.Sorted(maps.Keys(files)) {
		if p, _ := files[prefix].(string); !validEnvFile(p) {
			return m.invalidf(recallBulletFilesPath+"."+prefix, "%s is %s", FormatValue(p), envFileHint)
		}
	}
	return nil
}

// RecallSources is [recall] from the shared file in configDir, each path with a
// leading ~/ expanded through lookupEnv's HOME, the bullet files by prefix. No
// configDir, and no file or table in it, is no sources.
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
