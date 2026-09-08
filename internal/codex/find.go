package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Hit is one place a searched-for text was found.
type Hit struct {
	Path    string // rollout file
	Line    int    // 1-based line number within the file
	KeyPath string // dotted path to the key holding the value
	Value   string // the value, truncated
}

// Find locates a literal text inside rollout files and reports the key it lives
// under.
//
// This is the inverse of guessing. The rollout schema is undocumented, and
// guessing key names produced wrong answers: `title` and `name` matched tool
// names, config values and the title of a web page the agent had opened. But
// the user knows what the value should be — the chat is called "Konsole" — so
// the honest way to learn the key is to search for the value and report where
// it lives.
//
// Files are read end to end, and any file is searched, not only rollouts: the
// value being hunted may well live in whatever store the desktop app keeps
// beside them — the chat name shown in the app was not in a rollout at all.
func Find(dir, needle string, limit int) ([]Hit, error) {
	if strings.TrimSpace(needle) == "" {
		return nil, fmt.Errorf("нечего искать")
	}
	needleLow := strings.ToLower(needle)

	var hits []Hit
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if len(hits) >= limit {
			return filepath.SkipAll
		}
		found, ferr := findInFile(path, needleLow, limit-len(hits))
		if ferr == nil {
			hits = append(hits, found...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].KeyPath < hits[j].KeyPath })
	return hits, nil
}

func findInFile(path, needleLow string, limit int) ([]Hit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256<<10), maxLine)

	var hits []Hit
	for line := 1; sc.Scan(); line++ {
		if len(hits) >= limit {
			break
		}
		raw := sc.Text()
		// The cheap test first: only lines that contain the text at all are
		// worth parsing. Without this a directory of 200 MB rollouts would
		// take minutes.
		if !strings.Contains(strings.ToLower(raw), needleLow) {
			continue
		}
		var v any
		if json.Unmarshal([]byte(raw), &v) != nil {
			continue
		}
		walkPath(v, "", func(keyPath string, val any) {
			s, ok := val.(string)
			if !ok || !strings.Contains(strings.ToLower(s), needleLow) {
				return
			}
			hits = append(hits, Hit{
				Path: path, Line: line, KeyPath: keyPath,
				Value: clip(collapse(s), 90),
			})
		})
	}
	return hits, sc.Err()
}

// walkPath is walk with the dotted key path carried along, so a hit can say
// exactly where the value lives.
func walkPath(v any, prefix string, fn func(path string, val any)) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic output
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			fn(p, t[k])
			walkPath(t[k], p, fn)
		}
	case []any:
		for i, val := range t {
			p := fmt.Sprintf("%s[%d]", prefix, i)
			walkPath(val, p, fn)
		}
	}
}
