package appserver

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Activity is one thing the model did, distilled from a server notification to
// a single line worth reading. The point is debugging: while crescent drives a
// goal unattended, this is the only window into what the model is actually
// doing.
type Activity struct {
	ThreadID string
	// TurnID identifies the turn in progress. turn/interrupt needs it, so it
	// has to be captured as the stream goes by: there is no way to ask for it
	// afterwards.
	TurnID string
	Kind   string // короткая метка: сообщение, команда, правка, размышление…
	Text   string // одна строка, уже очищенная от переносов
}

// baseNote carries the fields every streaming notification shares.
type baseNote struct {
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId"`
	Delta     string          `json:"delta"`
	Item      json.RawMessage `json:"item"`
	Goal      json.RawMessage `json:"goal"`
	Error     json.RawMessage `json:"error"`
	WillRetry bool            `json:"willRetry"`
}

// item is the shared shape of a thread item: the discriminating `type` plus the
// few text-bearing fields different item kinds use. Unknown kinds still yield
// their type, so a new item type shows up in the log rather than vanishing.
type item struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Command string          `json:"command"`
	Content json.RawMessage `json:"content"`
	Status  string          `json:"status"`
}

// Interpret turns a notification method and its params into an Activity, or
// reports ok=false for notifications not worth a line (token deltas, diffs).
//
// Deltas are deliberately dropped: they arrive character by character and would
// drown the log. The completed item carries the whole text, and that is what
// gets written.
func Interpret(method string, params json.RawMessage) (Activity, bool) {
	var n baseNote
	_ = json.Unmarshal(params, &n)
	a := Activity{ThreadID: n.ThreadID, TurnID: n.TurnID}

	switch method {
	case "item/completed", "item/started":
		var it item
		if json.Unmarshal(n.Item, &it) != nil {
			return a, false
		}
		return itemActivity(a, method, it)

	case "thread/goal/updated":
		var g Goal
		_ = json.Unmarshal(n.Goal, &g)
		a.Kind = "цель"
		a.Text = "статус: " + orUnknown(g.Status)
		if g.Objective != "" {
			a.Text += " — " + oneLine(g.Objective)
		}
		return a, true

	case "thread/goal/cleared":
		a.Kind, a.Text = "цель", "снята"
		return a, true

	case "thread/status/changed":
		var s struct {
			Status ThreadStatus `json:"status"`
		}
		_ = json.Unmarshal(params, &s)
		a.Kind, a.Text = "статус", orUnknown(s.Status.Type)
		return a, true

	case "turn/started":
		a.Kind, a.Text = "ход", "начат"
		return a, true

	case "turn/completed":
		a.Kind, a.Text = "ход", "завершён"
		return a, true

	case "error":
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(n.Error, &e)
		a.Kind = "ошибка"
		a.Text = oneLine(e.Message)
		if n.WillRetry {
			a.Text += " (будет повтор)"
		}
		return a, true
	}
	return a, false
}

func itemActivity(a Activity, method string, it item) (Activity, bool) {
	// A started item is worth a line only for long-running kinds; otherwise the
	// completed event says the same thing with the result filled in.
	started := method == "item/started"

	switch it.Type {
	case "agentMessage":
		if started {
			return a, false
		}
		a.Kind, a.Text = "сообщение", oneLine(it.Text)
	case "reasoning":
		if started {
			return a, false
		}
		a.Kind, a.Text = "размышление", oneLine(it.Text)
	case "commandExecution":
		a.Kind = "команда"
		a.Text = oneLine(it.Command)
		if !started && it.Status != "" {
			a.Text += "  → " + it.Status
		}
	case "fileChange":
		a.Kind, a.Text = "правка", oneLine(summariseContent(it.Content))
	case "webSearch":
		a.Kind, a.Text = "поиск", oneLine(it.Text)
	case "mcpToolCall":
		a.Kind, a.Text = "инструмент", oneLine(it.Text)
	default:
		if started {
			return a, false
		}
		a.Kind, a.Text = it.Type, oneLine(it.Text)
	}
	if a.Text == "" && it.Status != "" {
		a.Text = it.Status
	}
	return a, a.Kind != ""
}

// summariseContent turns a file-change payload into a short description without
// dumping the whole diff.
func summariseContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var files []struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(raw, &files) == nil && len(files) > 0 {
		names := make([]string, 0, len(files))
		for _, f := range files {
			if f.Path != "" {
				names = append(names, f.Path)
			}
		}
		if len(names) > 0 {
			return strings.Join(names, ", ")
		}
	}
	return fmt.Sprintf("%d байт изменений", len(raw))
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 300 {
		return string(r[:299]) + "…"
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
