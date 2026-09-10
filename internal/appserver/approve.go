package appserver

import (
	"encoding/json"
	"strings"
)

// IsApprovalRequest reports whether a server request is asking permission.
//
// Codex asks for approval even when its own settings already allow the action —
// a live run had one worker prompting for every push while its neighbour, with
// identical settings, never did. Left unanswered the turn simply waits, which
// defeats the point of running unattended.
func IsApprovalRequest(method string) bool {
	return strings.Contains(method, "/requestApproval") ||
		method == "item/tool/requestUserInput"
}

// ApprovalAnswer is the reply that grants a request, in the shape its own
// schema requires.
//
// Command and file-change approvals answer with a decision; acceptForSession
// grants the rest of the session too, so one prompt does not become twenty.
// Permission requests answer with a scope instead, and turn is the narrowest
// grant that lets the turn proceed.
func ApprovalAnswer(method string, params json.RawMessage) any {
	switch {
	case strings.Contains(method, "permissions/requestApproval"):
		return map[string]any{"scope": "turn"}
	case method == "item/tool/requestUserInput":
		return map[string]any{"response": "да"}
	default:
		return map[string]any{"decision": "acceptForSession"}
	}
}

// ApprovalSummary describes a request in one line, for the journal.
func ApprovalSummary(method string, params json.RawMessage) string {
	var p struct {
		Command []string `json:"command"`
		Reason  string   `json:"reason"`
		Cwd     string   `json:"cwd"`
	}
	_ = json.Unmarshal(params, &p)

	what := strings.TrimPrefix(method, "item/")
	what = strings.TrimSuffix(what, "/requestApproval")
	if len(p.Command) > 0 {
		what += ": " + strings.Join(p.Command, " ")
	}
	if p.Reason != "" {
		what += " (" + p.Reason + ")"
	}
	return what
}
