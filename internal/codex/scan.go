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
	"bufio"
	"encoding/json"
	"errors"
	"net/url"
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

// Reset is one usage-limit window found in a rollout, with the key it came
// from. Codex records more than one window per snapshot — a rolling 5-hour one
// and a weekly one — so a single "the reset time" field would be a coin flip.
type Reset struct {
	At  time.Time
	Key string // the JSON key it was read from, for diagnostics
}

// Session is one rollout file, summarised.
type Session struct {
	ID       string // this rollout's own session UUID
	ParentID string // the thread it was forked or continued from, if any
	Path     string // rollout file
	Cwd      string // working directory, normalised to a filesystem path

	Name      string     // the conversation's own title, as shown in the app
	Objective string     // the goal text, empty when the session has no goal
	Status    GoalStatus // last goal status seen in the file

	Resets   []Reset   // every usage-limit window found, in file order
	Modified time.Time // file mtime — the "is someone working here?" signal
	Size     int64

	// Provenance, so `-dump` can say where a value came from. The rollout
	// schema is undocumented; knowing which key produced a suspicious value is
	// what makes a report actionable.
	ObjectiveKey string
	IDKey        string
	NameKey      string

	// NameRank is exported only so the cache can round-trip it; it is not part
	// of the API anyone should read.
	NameRank int `json:"nameRank,omitempty"`
}

// ResetsAt returns the nearest reset still in the future, or the zero time if
// every window found has already lapsed. An old rollout keeps whatever window
// was current when it last ran, so a past value says nothing about now.
func (s Session) ResetsAt() time.Time {
	var best time.Time
	now := time.Now()
	for _, r := range s.Resets {
		if r.At.After(now) && (best.IsZero() || r.At.Before(best)) {
			best = r.At
		}
	}
	return best
}

// HasGoal reports whether a goal record was found at all.
func (s Session) HasGoal() bool { return s.Objective != "" || s.Status != StatusUnknown }

// Label is what a human should see: the conversation's own title, which is
// usually a short summary someone can recognise, followed by the goal text.
// The title alone is not enough — several chats can share one — and the goal
// alone is what the list showed before, which reads as a wall of first
// sentences.
func (s Session) Label() string {
	if s.Name == "" {
		return s.Title()
	}
	full := collapse(s.Name) + " — " + collapse(s.Objective)
	if s.Objective == "" {
		full = collapse(s.Name)
	}
	return clip(full, 78)
}

// Title is a short single-line label for the UI. Objectives are free-form user
// text: they arrive with newlines and markdown escaping, neither of which
// belongs in a list row.
func (s Session) Title() string {
	t := s.Objective
	if t == "" {
		t = "(без цели) " + filepath.Base(s.Path)
	}
	return clip(collapse(t), 70)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// collapse turns any run of whitespace into one space and drops markdown
// backslash escaping, so `docs/PLAN\_A.md` reads as written.
func collapse(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	var b strings.Builder
	b.Grow(len(s))
	prevEsc := false
	for _, r := range s {
		if r == '\\' && !prevEsc {
			prevEsc = true
			continue
		}
		b.WriteRune(r)
		prevEsc = false
	}
	return b.String()
}

// Scan walks the sessions directory and summarises every rollout file.
// Files that cannot be read or parsed are skipped, never fatal: one corrupt
// rollout must not hide the rest.
func Scan(dir string) ([]Session, error) { return ScanWithProgress(dir, nil) }

// ScanWithProgress is Scan with a callback for the first, slow sweep.
func ScanWithProgress(dir string, report Progress) ([]Session, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("codex: sessions path is not a directory: " + dir)
	}

	var files []string
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, keep going
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(path), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	c := loadCache()
	defer c.save()

	out := make([]Session, 0, len(files))
	for i, path := range files {
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		s, ok := c.get(path, fi)
		if !ok {
			if report != nil {
				report(i, len(files), path)
			}
			s, err = ScanFile(path)
			if err != nil {
				continue
			}
			c.put(path, fi, s)
		}
		s.Path = path
		s.Modified, s.Size = fi.ModTime(), fi.Size()
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// ScanFile summarises a single rollout file.
func ScanFile(path string) (Session, error) {
	return scanWhole(path)
}

// applyFilename derives identity from the rollout's name.
//
// Rollout names look like rollout-<ts>-<uuid>.jsonl or, for a session continued
// from another thread, rollout-<ts>-<parent>_<own>.jsonl. Taking the first UUID
// reports the parent, which makes distinct files collide on one id — visible in
// the wild as two rollouts claiming the same session.
func (s *Session) applyFilename() {
	ids := uuidRe.FindAllString(filepath.Base(s.Path), -1)
	if len(ids) == 0 {
		return
	}
	s.ID = ids[len(ids)-1]
	s.IDKey = "filename"
	if len(ids) > 1 && ids[0] != s.ID {
		s.ParentID = ids[0]
	}
}

func (s *Session) applyRecord(v any) {
	// Windows are collected per record and then replace what came before: a
	// long rollout holds a snapshot from every turn, so accumulating them mixes
	// windows that lapsed weeks ago with the current ones. Only the newest
	// record in the file describes the limits as Codex last saw them.
	var windows []Reset

	walk(v, func(key string, val any) {
		switch normKey(key) {
		case "cwd", "workingdirectory", "workdir":
			if str, ok := val.(string); ok && str != "" {
				s.Cwd = normalizePath(str)
			}
		case "id", "sessionid", "threadid", "conversationid":
			// The file name wins. An id recorded inside a rollout is not
			// necessarily the rollout's own: transcripts reference other
			// sessions — spawned agents, resumed threads — and taking the last
			// one seen made three unrelated files claim one another's identity,
			// with two of them colliding outright. An in-file id is used only
			// when the name carries none.
			if str, ok := val.(string); ok && uuidRe.MatchString(str) && s.ID == "" {
				s.ID, s.IDKey = str, key
			}
		case "objective", "goaltext", "goalobjective":
			if str, ok := val.(string); ok && str != "" {
				s.setObjective(str, key)
			}
		case "goal":
			switch g := val.(type) {
			case string:
				if g != "" {
					s.setObjective(g, key)
				}
			case map[string]any:
				for k, gv := range g {
					switch normKey(k) {
					case "objective", "text", "prompt", "description":
						if str, ok := gv.(string); ok && str != "" {
							s.setObjective(str, key+"."+k)
						}
					case "status", "state":
						if str, ok := gv.(string); ok && str != "" {
							s.Status = GoalStatus(str)
						}
					}
				}
			}
		// Only qualified keys. On real data the bare ones matched a config
		// value ("exec", "auto") and the title of a web page the agent had
		// opened, so the list showed confident nonsense. Until `crescent -find`
		// identifies the real key, no name beats a wrong name.
		case "chattitle", "threadtitle", "conversationtitle", "sessiontitle":
			s.setName(val, key, 3)
		case "goalstatus":
			if str, ok := val.(string); ok && str != "" {
				s.Status = GoalStatus(str)
			}
		case "resetsat", "resetat", "resetsatunix", "resettime":
			if t, ok := parseTime(val); ok {
				windows = addReset(windows, t, key)
			}
		case "resetsinseconds", "resetafterseconds":
			if secs, ok := toFloat(val); ok && secs > 0 {
				// Relative offsets are anchored to now, not to file mtime: an
				// anchor that is too early only makes crescent wait longer,
				// while too late would make it hammer a still-closed window.
				windows = addReset(windows, time.Now().Add(time.Duration(secs)*time.Second), key)
			}
		}
	})

	if len(windows) > 0 {
		sort.Slice(windows, func(i, j int) bool { return windows[i].At.Before(windows[j].At) })
		s.Resets = windows
	}
}

// setName records the conversation title. Ranking matters because `title` and
// especially `name` are generic enough to appear on unrelated things inside a
// transcript — a tool, an item, a file. A qualified key always wins over a bare
// one, and within the same rank the newest record wins, since the app renames a
// chat once it has seen a few turns.
func (s *Session) setName(val any, key string, rank int) {
	text, ok := val.(string)
	if !ok {
		return
	}
	text = strings.TrimSpace(text)
	// A conversation title is a short human phrase. Anything long is prose that
	// happened to sit under a generic key.
	if text == "" || len([]rune(text)) > 120 || strings.ContainsAny(text, "\n\r") {
		return
	}
	if rank < s.NameRank {
		return
	}
	s.Name, s.NameKey, s.NameRank = text, key, rank
}

// setObjective records the goal text and where it came from. walk visits a
// nested object both through its parent key and again through its own keys, and
// Go randomises map iteration order, so the qualified path is pinned once found
// — otherwise the reported provenance would differ between runs.
func (s *Session) setObjective(text, key string) {
	s.Objective = text
	if s.ObjectiveKey == "" || !strings.Contains(s.ObjectiveKey, ".") {
		s.ObjectiveKey = key
	}
}

// addReset appends a window unless the same instant is already present: one
// record mentions each window more than once.
func addReset(windows []Reset, t time.Time, key string) []Reset {
	for _, r := range windows {
		if r.At.Equal(t) {
			return windows
		}
	}
	return append(windows, Reset{At: t, Key: key})
}

// normalizePath turns what Codex records as a working directory into a path
// that can actually be passed to a process. Some records hold a plain path,
// others a file:// URL, and those are percent-encoded — a cwd with non-ASCII
// characters arrives as file:///home/u/%D0%94%D0%BE%D0%BA... and is useless
// until decoded.
func normalizePath(s string) string {
	if !strings.HasPrefix(s, "file://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	if p := u.Path; p != "" {
		return filepath.FromSlash(p)
	}
	return s
}

// Walk visits every key/value pair in a decoded JSON document. Exported because
// the same schema-tolerant approach is needed for the codex --json event
// stream, which is as undocumented as the rollout format.
func Walk(v any, fn func(key string, val any)) { walk(v, fn) }

// ParseTime accepts what a timestamp field has been seen to hold: an RFC3339
// string, or a number of seconds, milliseconds, microseconds or nanoseconds
// since the epoch.
func ParseTime(v any) (time.Time, bool) { return parseTime(v) }

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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256<<10), maxLine)

	keys := map[string]int{}
	for sc.Scan() {
		var v any
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		walk(v, func(k string, _ any) { keys[k]++ })
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

// AccountLimits reports the usage-limit windows that are actually in force.
//
// This cannot be read off an individual session. The limit is per account, not
// per session, and a rollout keeps whatever snapshot was current the last time
// that session ran — which is why a directory of old sessions is full of reset
// times that lapsed days ago. Only the most recently written rollout carries a
// snapshot worth believing.
func AccountLimits(sessions []Session) (from Session, windows []Reset, ok bool) {
	var newest *Session
	for i := range sessions {
		if len(sessions[i].Resets) == 0 {
			continue
		}
		if newest == nil || sessions[i].Modified.After(newest.Modified) {
			newest = &sessions[i]
		}
	}
	if newest == nil {
		return Session{}, nil, false
	}
	windows = append(windows, newest.Resets...)
	sort.Slice(windows, func(i, j int) bool { return windows[i].At.Before(windows[j].At) })
	return *newest, windows, true
}

// NextReset returns the nearest window still ahead of us, across the whole
// account. Zero means nothing is currently limited, as far as the files show.
func NextReset(sessions []Session) time.Time {
	_, windows, ok := AccountLimits(sessions)
	if !ok {
		return time.Time{}
	}
	now := time.Now()
	for _, w := range windows {
		if w.At.After(now) {
			return w.At
		}
	}
	return time.Time{}
}
