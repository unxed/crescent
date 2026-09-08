package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Reading only the head and tail of a rollout was wrong, and wrong in exactly
// the case crescent exists for. Goal records are written whenever the goal is
// set or edited, which in a session running for days lands in the middle of the
// file: the active Konsole goal sat at line 30255 of its rollout and the
// scanner never saw it. Long sessions are the ones that hit usage limits, so
// the scanner was blindest precisely where it matters.
//
// So files are now read end to end. Two things keep that affordable:
//
//   - a byte-level prefilter. Only lines containing a marker are handed to the
//     JSON parser, and in a rollout of tool output that is a tiny fraction.
//   - a cache keyed by size and modification time. A rollout that has not
//     changed is never read twice, so only the first run pays for the sweep and
//     only growing sessions are re-read afterwards.
var markers = [][]byte{
	[]byte("goal"),
	[]byte("objective"),
	[]byte("rate_limit"),
	[]byte("rateLimit"),
	[]byte("resets"),
	[]byte("cwd"),
	[]byte("working"),
	[]byte("title"),
	[]byte("session_meta"),
	[]byte("session_id"),
	[]byte("thread_id"),
}

func interesting(line []byte) bool {
	for _, m := range markers {
		if bytes.Contains(line, m) {
			return true
		}
	}
	return false
}

// maxLine bounds one JSON record. Tool output can carry a whole file, and the
// default scanner limit would split such a record into invalid fragments.
const maxLine = 64 << 20

// scanWhole reads a rollout end to end, applying every record it recognises.
func scanWhole(path string) (Session, error) {
	s := Session{Path: path}
	s.applyFilename()

	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256<<10), maxLine)

	for sc.Scan() {
		line := sc.Bytes()
		if !interesting(line) {
			continue
		}
		var v any
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		s.applyRecord(v)
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		// A single over-long record must not discard everything already read.
		return s, nil
	}
	return s, nil
}

// ---------------------------------------------------------------- the cache

type cacheEntry struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Session Session   `json:"session"`
}

type cache struct {
	path    string
	Entries map[string]cacheEntry `json:"entries"`
	dirty   bool
}

// CacheDir is where the index lives. It is a cache in the strict sense:
// deleting it costs one slow scan and nothing else.
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "crescent"), nil
}

func loadCache() *cache {
	c := &cache{Entries: map[string]cacheEntry{}}
	dir, err := CacheDir()
	if err != nil {
		return c
	}
	c.path = filepath.Join(dir, "index.json")

	data, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var onDisk cache
	if json.Unmarshal(data, &onDisk) != nil || onDisk.Entries == nil {
		return c
	}
	c.Entries = onDisk.Entries
	return c
}

func (c *cache) save() {
	if !c.dirty || c.path == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(c.path), 0o755) != nil {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	// Written through a temporary file: a half-written index would be read
	// back as garbage on the next run.
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, c.path)
	}
}

func (c *cache) get(path string, fi os.FileInfo) (Session, bool) {
	e, ok := c.Entries[path]
	if !ok || e.Size != fi.Size() || !e.ModTime.Equal(fi.ModTime()) {
		return Session{}, false
	}
	return e.Session, true
}

func (c *cache) put(path string, fi os.FileInfo, s Session) {
	c.Entries[path] = cacheEntry{Size: fi.Size(), ModTime: fi.ModTime(), Session: s}
	c.dirty = true
}

// Progress reports how a slow first sweep is going. Nil means stay quiet.
type Progress func(done, total int, path string)

// DropCache removes the index, forcing the next scan to read everything again.
func DropCache() error {
	dir, err := CacheDir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, "index.json"))
}

// CacheStatus reports how many rollouts are already indexed.
func CacheStatus() (entries int, path string) {
	c := loadCache()
	return len(c.Entries), c.path
}

var errNotDir = fmt.Errorf("codex: sessions path is not a directory")
