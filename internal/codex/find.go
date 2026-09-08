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
	InScan  bool   // whether it sits in the region the scanner actually reads
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
// deep controls how much of each file is read. Without it only the region the
// scanner itself reads is searched, which answers "why does the scanner not see
// this?". With it the whole file is streamed, which answers "is it in the file
// at all?" and costs a full read of some very large rollouts.
func Find(dir, needle string, deep bool, limit int) ([]Hit, error) {
	if strings.TrimSpace(needle) == "" {
		return nil, fmt.Errorf("нечего искать")
	}
	needleLow := strings.ToLower(needle)

	var hits []Hit
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".jsonl") {
			return nil
		}
		if len(hits) >= limit {
			return filepath.SkipAll
		}
		found, ferr := findInFile(path, needleLow, deep, limit-len(hits))
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

func findInFile(path, needleLow string, deep bool, limit int) ([]Hit, error) {
	scanRegion, err := scannedLineCount(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

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
		if !deep && !scanRegion(line, int64(len(raw))) {
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
				Value: clip(collapse(s), 90), InScan: scanRegion(line, int64(len(raw))),
			})
		})
	}
	return hits, sc.Err()
}

// scannedLineCount returns a predicate telling whether a line falls inside the
// head or tail the scanner reads. Approximated by running byte offset, which is
// what the scanner's own windows are based on.
func scannedLineCount(path string) (func(line int, size int64) bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	total := fi.Size()
	var offset int64
	return func(_ int, size int64) bool {
		start := offset
		offset += size + 1
		return start < headTail || start > total-headTail
	}, nil
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
