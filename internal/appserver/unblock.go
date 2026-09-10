package appserver

import (
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
	for _, marker := range []string{
		"для возобновления", "напишите", "напиши", "подтвердите",
		"требует явного разрешения", "waiting for your", "reply with", "confirm",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}
