// Package codex reads what the Codex CLI and app leave on disk.
//
// Everything here is deliberately defensive. The rollout format is internal to
// Codex, undocumented, and has already changed at least once (reset times moved
// from relative offsets to absolute timestamps in openai/codex#5304). So the
// parser never assumes a schema: it walks each JSON record and picks up keys it
// recognises, under several spellings, wherever they appear. A field it cannot
// find is reported as missing rather than guessed.
//
// If the format moves again, `crescent -dump` prints the key names actually
// present (names only, never values) so the recogniser can be extended without
// anyone having to hand over a transcript.
package codex

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Home returns the Codex state directory: $CODEX_HOME if set, else ~/.codex.
// On Windows that lands in %USERPROFILE%\.codex, which is where Codex puts it.
func Home() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// SessionsDir is where rollout files live.
func SessionsDir() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "sessions"), nil
}

// GoalStatus mirrors the states Codex's goal engine can be in. Codex added
// `blocked` and `usageLimited` in openai/codex#23094; unknown values are kept
// verbatim rather than being forced into this set.
type GoalStatus string

// Known goal states.
const (
	StatusUnknown      GoalStatus = ""
	StatusActive       GoalStatus = "active"
	StatusPaused       GoalStatus = "paused"
	StatusBlocked      GoalStatus = "blocked"
	StatusUsageLimited GoalStatus = "usageLimited"
	StatusComplete     GoalStatus = "complete"
)

// Resumable reports whether this is a goal the relay could push forward.
// A completed goal is done; a blocked one needs a human, not a retry.
func (s GoalStatus) Resumable() bool {
	switch GoalStatus(strings.ToLower(string(s))) {
	case "complete", "completed", "blocked", "cancelled", "canceled":
		return false
	}
	return true
}

// Session is one rollout file, summarised.
type Session struct {
	ID        string     // session/thread UUID
	Path      string     // rollout file
	Cwd       string     // working directory the session was started in
	Objective string     // the goal text, empty when the session has no goal
	Status    GoalStatus // last goal status seen in the file
	ResetsAt  time.Time  // when the usage limit lifts; zero if unknown
	Modified  time.Time  // file mtime — the "is someone working here?" signal
	Size      int64
}

// HasGoal reports whether a goal record was found at all.
func (s Session) HasGoal() bool { return s.Objective != "" || s.Status != StatusUnknown }

// Title is a short label for the UI.
func (s Session) Title() string {
	t := s.Objective
	if t == "" {
		t = "(без цели) " + filepath.Base(s.Path)
	}
	if len([]rune(t)) > 70 {
		t = string([]rune(t)[:69]) + "…"
	}
	return t
}

// Scan walks the sessions directory and summarises every rollout file.
// Files that cannot be read or parsed are skipped, never fatal: one corrupt
// rollout must not hide the rest.
func Scan(dir string) ([]Session, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("codex: sessions path is not a directory: " + dir)
	}

	var out []Session
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, keep going
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		s, err := ScanFile(path)
		if err != nil {
			return nil
		}
		s.Modified, s.Size = fi.ModTime(), fi.Size()
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// headTail bounds how much of a rollout is read. Sessions can reach hundreds of
// megabytes; identity lives at the top and current state at the bottom, so the
// middle is never worth the I/O.
const headTail = 256 << 10

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// ScanFile summarises a single rollout file.
func ScanFile(path string) (Session, error) {
	s := Session{Path: path}
	if m := uuidRe.FindString(filepath.Base(path)); m != "" {
		s.ID = m
	}

	head, tail, err := readEnds(path, headTail)
	if err != nil {
		return s, err
	}

	// The head carries identity, the tail carries current state; later records
	// win, so the head is applied first and then overwritten by the tail.
	for _, chunk := range [][]byte{head, tail} {
		for _, line := range splitLines(chunk) {
			var v any
			if json.Unmarshal(line, &v) != nil {
				continue // truncated first/last line of a chunk, or not JSON
			}
			s.applyRecord(v)
		}
	}
	return s, nil
}

func (s *Session) applyRecord(v any) {
	walk(v, func(key string, val any) {
		switch normKey(key) {
		case "cwd", "workingdirectory", "workdir":
			if str, ok := val.(string); ok && str != "" {
				s.Cwd = str
			}
		case "id", "sessionid", "threadid", "conversationid":
			if str, ok := val.(string); ok && uuidRe.MatchString(str) && s.ID == "" {
				s.ID = str
			}
		case "objective", "goaltext", "goalobjective":
			if str, ok := val.(string); ok && str != "" {
				s.Objective = str
			}
		case "goal":
			switch g := val.(type) {
			case string:
				if g != "" {
					s.Objective = g
				}
			case map[string]any:
				for k, gv := range g {
					switch normKey(k) {
					case "objective", "text", "prompt", "description":
						if str, ok := gv.(string); ok && str != "" {
							s.Objective = str
						}
					case "status", "state":
						if str, ok := gv.(string); ok && str != "" {
							s.Status = GoalStatus(str)
						}
					}
				}
			}
		case "goalstatus":
			if str, ok := val.(string); ok && str != "" {
				s.Status = GoalStatus(str)
			}
		case "resetsat", "resetat", "resetsatunix", "resettime":
			if t, ok := parseTime(val); ok {
				s.ResetsAt = t
			}
		case "resetsinseconds", "resetafterseconds":
			if secs, ok := toFloat(val); ok && secs > 0 {
				// Relative offsets are anchored to now, not to file mtime: the
				// caller reads this immediately after Codex wrote it, and an
				// anchor that is too early only makes the relay wait longer.
				s.ResetsAt = time.Now().Add(time.Duration(secs) * time.Second)
			}
		}
	})
}

// walk visits every key/value pair in a decoded JSON document.
func walk(v any, fn func(key string, val any)) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			fn(k, val)
			walk(val, fn)
		}
	case []any:
		for _, val := range t {
			walk(val, fn)
		}
	}
}

// Keys returns every distinct key name in a rollout file, with how often it
// occurred. Values are never collected — this is what makes it safe to paste a
// -dump report into a bug report.
func Keys(path string) (map[string]int, error) {
	head, tail, err := readEnds(path, headTail)
	if err != nil {
		return nil, err
	}
	keys := map[string]int{}
	for _, chunk := range [][]byte{head, tail} {
		for _, line := range splitLines(chunk) {
			var v any
			if json.Unmarshal(line, &v) != nil {
				continue
			}
			walk(v, func(k string, _ any) { keys[k]++ })
		}
	}
	return keys, nil
}

func normKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "_", "")
	k = strings.ReplaceAll(k, "-", "")
	return k
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// parseTime accepts what the field has actually been seen to hold: an RFC3339
// string, or a number in seconds, milliseconds or microseconds since the epoch.
func parseTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts, true
			}
		}
	case float64:
		return epochToTime(t), true
	}
	return time.Time{}, false
}

// epochToTime guesses the unit from magnitude. The thresholds sit far from any
// plausible real timestamp: seconds since 1970 passed 10^9 in 2001 and will not
// reach 10^11 until the year 5138.
func epochToTime(n float64) time.Time {
	switch {
	case n > 1e17: // nanoseconds
		return time.Unix(0, int64(n))
	case n > 1e14: // microseconds
		return time.UnixMicro(int64(n))
	case n > 1e11: // milliseconds
		return time.UnixMilli(int64(n))
	default:
		return time.Unix(int64(n), 0)
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, []byte(l))
		}
	}
	return out
}

// readEnds returns up to n bytes from the start and n bytes from the end.
func readEnds(path string, n int64) (head, tail []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := fi.Size()

	if size <= n {
		b, err := io.ReadAll(f)
		return b, nil, err
	}
	head = make([]byte, n)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, nil, err
	}
	tail = make([]byte, n)
	if _, err := f.ReadAt(tail, size-n); err != nil && err != io.EOF {
		return head, nil, err
	}
	return head, tail, nil
}
