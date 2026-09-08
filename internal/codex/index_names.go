package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IndexFile is the app's own list of conversations, kept beside the sessions
// directory rather than inside it.
const IndexFile = "session_index.jsonl"

// IndexEntry is what the index knows about one conversation.
type IndexEntry struct {
	Name string // the chat name shown in the app
}

// LoadIndex reads the conversation names the app maintains.
//
// Chat names are not in the rollouts. Guessing at rollout keys produced tool
// names and the title of a web page the agent had opened; the reverse search
// found the real value under `thread_name` in session_index.jsonl, a file
// beside the sessions directory. So names come from there, and only from there.
//
// A missing or unreadable index is not an error: names are a nicety, and every
// other part of crescent works without them.
func LoadIndex(home string) map[string]IndexEntry {
	out := map[string]IndexEntry{}

	f, err := os.Open(filepath.Join(home, IndexFile))
	if err != nil {
		return out
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)

	for sc.Scan() {
		var v any
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		name, ids := indexRecord(v)
		if name == "" {
			continue
		}
		for _, id := range ids {
			out[id] = IndexEntry{Name: name}
		}
	}
	return out
}

// indexRecord pulls the name and the session ids out of one index line.
//
// Which id field the index uses is not documented either, so any UUID in the
// record is accepted — but qualified keys are collected first, and a record's
// own id is what matters. Mapping every UUID found is safe here because the
// lookup is by the session we already know.
func indexRecord(v any) (name string, ids []string) {
	var qualified []string
	walk(v, func(key string, val any) {
		s, ok := val.(string)
		if !ok {
			return
		}
		switch normKey(key) {
		case "threadname", "conversationname", "chatname", "sessionname":
			if name == "" || len(s) > 0 {
				name = strings.TrimSpace(s)
			}
		case "id", "sessionid", "threadid", "conversationid", "rolloutid":
			if uuidRe.MatchString(s) {
				qualified = append(qualified, uuidRe.FindString(s))
			}
		default:
			if uuidRe.MatchString(s) {
				ids = append(ids, uuidRe.FindString(s))
			}
		}
	})
	if len(qualified) > 0 {
		return name, qualified
	}
	return name, ids
}

// IndexKeys reports the key names present in the index, with counts. Names
// only, never values — the same rule that makes a -dump report safe to paste.
func IndexKeys(home string) (map[string]int, error) {
	f, err := os.Open(filepath.Join(home, IndexFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)

	keys := map[string]int{}
	for sc.Scan() {
		var v any
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		walk(v, func(k string, _ any) { keys[k]++ })
	}
	return keys, sc.Err()
}

// SortedKeys returns key names ordered by how often they occur.
func SortedKeys(keys map[string]int) []string {
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if keys[out[i]] != keys[out[j]] {
			return keys[out[i]] > keys[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// applyIndex fills in chat names. It runs after the cache, deliberately: the
// index changes on its own schedule, so a name must never be frozen into a
// session's cached entry.
func applyIndex(sessions []Session, index map[string]IndexEntry) {
	if len(index) == 0 {
		return
	}
	for i := range sessions {
		e, ok := index[sessions[i].ID]
		if !ok && sessions[i].ParentID != "" {
			e, ok = index[sessions[i].ParentID]
		}
		if ok && e.Name != "" {
			sessions[i].Name = e.Name
			sessions[i].NameKey = IndexFile
		}
	}
}
