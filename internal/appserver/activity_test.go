package appserver

import "testing"

// The point of Activity is debugging a run left unattended, so the shapes here
// are taken from the schema: item is discriminated by type, notifications carry
// threadId, deltas are noise and must be dropped.
func TestInterpretDistilsTheStream(t *testing.T) {
	cases := []struct {
		method, params, kind, text string
		ok                         bool
	}{
		{"item/completed", `{"threadId":"t1","item":{"type":"agentMessage","text":"готово"}}`, "сообщение", "готово", true},
		{"item/completed", `{"threadId":"t1","item":{"type":"commandExecution","command":"go build","status":"completed"}}`, "команда", "go build  → completed", true},
		{"turn/started", `{"threadId":"t1"}`, "ход", "начат", true},
		{"error", `{"threadId":"t1","error":{"message":"boom"},"willRetry":true}`, "ошибка", "boom (будет повтор)", true},
		// deltas arrive character by character; they would drown the log
		{"item/agentMessage/delta", `{"threadId":"t1","delta":"g"}`, "", "", false},
		// a started message says nothing the completed one will not
		{"item/started", `{"threadId":"t1","item":{"type":"agentMessage"}}`, "", "", false},
	}
	for _, c := range cases {
		a, ok := Interpret(c.method, []byte(c.params))
		if ok != c.ok {
			t.Errorf("%s: ok=%v, want %v", c.method, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if a.Kind != c.kind || a.Text != c.text {
			t.Errorf("%s: got [%s] %q, want [%s] %q", c.method, a.Kind, a.Text, c.kind, c.text)
		}
		if a.ThreadID != "t1" {
			t.Errorf("%s: threadId lost", c.method)
		}
	}
}

// A new item type must still surface, not vanish.
func TestUnknownItemTypeStillLogs(t *testing.T) {
	a, ok := Interpret("item/completed", []byte(`{"threadId":"t1","item":{"type":"somethingNew","text":"x"}}`))
	if !ok || a.Kind != "somethingNew" {
		t.Errorf("got ok=%v kind=%q", ok, a.Kind)
	}
}
