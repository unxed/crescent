package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Codex blocks a goal in two ways. One is a protocol request, answered by
// ApprovalAnswer. The other is plain speech: the goal stops, its status becomes
// blocked, and its last message tells the person what to write back —
//
//	Цель заблокирована после трёх повторных попыток: PR #1030 проверен…
//	Для возобновления напишите: «мержи PR #1030 и удаляй ветку».
//
// The phrase is not fixed and cannot be guessed; it has to be read out of the
// message. These patterns cover the ways the request is phrased, in both
// languages, and each takes the quoted text that follows.
var unblockPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:напиши(?:те)?|ответьте|подтвердите|скажите)[^«"„]*[«"„]([^»"“]{2,300})[»"“]`),
	regexp.MustCompile(`(?i)(?:reply|respond|write|answer|confirm)[^«"„]*["“«]([^"”»]{2,300})["”»]`),
	regexp.MustCompile(`(?i)для возобновления[^«"„]*[«"„]([^»"“]{2,300})[»"“]`),
}

// UnblockPhrase returns the words a blocked goal is asking to hear, if its
// message names them.
//
// Returning nothing is the honest answer when the message does not quote a
// phrase: sending an invented one would be answering a question we did not read.
func UnblockPhrase(message string) string {
	msg := strings.TrimSpace(message)
	if msg == "" {
		return ""
	}
	for _, re := range unblockPatterns {
		if m := re.FindStringSubmatch(msg); len(m) > 1 {
			if phrase := strings.TrimSpace(m[1]); phrase != "" {
				return phrase
			}
		}
	}
	return ""
}

// AsksForConfirmation reports whether a message is waiting on a person at all,
// even when no exact phrase is quoted. Used to tell "stopped and waiting" from
// "stopped and finished".
func AsksForConfirmation(message string) bool {
	low := strings.ToLower(message)
	// Word stems, not whole words: a message says «подтвердите», «требуется
	// ваше подтверждение» and «требует явного разрешения» for the same thing,
	// and matching only the imperative missed two of the three.
	for _, marker := range []string{
		"для возобновления", "напишите", "напиши", "ответьте",
		"подтвержд", "разрешени", "разрешите", "заблокирован",
		"waiting for your", "reply with", "confirm", "approval", "permission",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// RecentAgentMessages returns the goal's recent messages, newest first.
//
// Reading only the newest was wrong: crescent's own restart makes the model
// answer, so the newest message is that answer — "Продолжаю текущую цель с того
// же места…" — while the explanation of the block sits a few messages back. The
// request has to be looked for, not assumed to be last.
func (c *Client) RecentAgentMessages(ctx context.Context, threadID string, n int) ([]string, error) {
	raw, err := c.Call(ctx, "thread/items/list", map[string]any{
		"threadId":      threadID,
		"limit":         n,
		"sortDirection": "desc",
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			Type    string    `json:"type"`
			Text    string    `json:"text"`
			Item    *listItem `json:"item"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("разбор ответа thread/items/list: %w", err)
	}

	var out []string
	kinds := map[string]int{}
	for _, it := range resp.Data {
		typ, text := it.Type, it.Text
		if it.Item != nil {
			typ, text = it.Item.Type, it.Item.Text
		}
		if text == "" {
			for _, cpart := range it.Content {
				text += cpart.Text
			}
		}
		kinds[typ]++
		if typ == "agentMessage" && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("среди %d элементов нет сообщений модели (виды: %v)",
			len(resp.Data), kinds)
	}
	return out, nil
}

// LastAgentMessage reads what a goal said last, straight from the thread.
//
// crescent only remembers messages it saw live, so a goal that blocked before
// this run started left nothing to read — and the words it asked for are only
// in that message. thread/items/list is the documented way to fetch them, newest
// first, without touching files or guessing.
func (c *Client) LastAgentMessage(ctx context.Context, threadID string) (string, error) {
	raw, err := c.Call(ctx, "thread/items/list", map[string]any{
		"threadId":      threadID,
		"limit":         20,
		"sortDirection": "desc",
	})
	if err != nil {
		return "", err
	}
	// Two shapes are accepted because the wire is what decides, not my reading
	// of it: items may arrive bare or wrapped, and guessing one and silently
	// returning nothing is how this went unnoticed for a round.
	var resp struct {
		Data []struct {
			Type    string    `json:"type"`
			Text    string    `json:"text"`
			Item    *listItem `json:"item"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("разбор ответа thread/items/list: %w", err)
	}
	if len(resp.Data) == 0 {
		return "", fmt.Errorf("thread/items/list вернул пусто")
	}

	kinds := map[string]int{}
	for _, it := range resp.Data {
		typ, text := it.Type, it.Text
		if it.Item != nil {
			typ, text = it.Item.Type, it.Item.Text
		}
		if text == "" {
			for _, c := range it.Content {
				text += c.Text
			}
		}
		kinds[typ]++
		if typ == "agentMessage" && strings.TrimSpace(text) != "" {
			return text, nil
		}
	}
	return "", fmt.Errorf("среди %d элементов нет сообщения модели (виды: %v)",
		len(resp.Data), kinds)
}

// listItem is the wrapped form of an item in a listing.
type listItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
