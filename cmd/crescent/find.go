package main

import (
	"fmt"
	"path/filepath"

	"github.com/unxed/crescent/internal/codex"
)

// runFind answers "which JSON key holds this?" by searching for a value the
// user can name.
//
// Guessing key names from the outside produced confident nonsense on real data:
// `title` and `name` matched a config value, a tool name and the title of a web
// page the agent had opened, so the goal list showed "exec", "auto" and a
// GitHub page title as chat names. But the user can see the real name in the
// app. Searching for that value and reporting where it lives turns the question
// around, and does it without anyone having to hand over a transcript.
func runFind(root, needle string) error {
	fmt.Printf("Ищу %q в %s\n\n", needle, root)

	hits, err := codex.Find(root, needle, 200)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("Не найдено.")
		fmt.Println("С -deep поиск идёт по всему каталогу Codex, а не только по сессиям:")
		fmt.Println("значение может храниться не в rollout-ах, а в собственных данных приложения.")
		return nil
	}

	// The same key repeated across many files is one answer, not many.
	byKey := map[string][]codex.Hit{}
	var order []string
	for _, h := range hits {
		if _, seen := byKey[h.KeyPath]; !seen {
			order = append(order, h.KeyPath)
		}
		byKey[h.KeyPath] = append(byKey[h.KeyPath], h)
	}

	fmt.Printf("Найдено под %d ключами:\n", len(order))
	for _, k := range order {
		hs := byKey[k]
		fmt.Printf("\n  %s   (совпадений: %d)\n", k, len(hs))
		fmt.Printf("      значение: %s\n", hs[0].Value)
		fmt.Printf("      файл:     %s, строка %d\n", filepath.Base(hs[0].Path), hs[0].Line)
	}
	return nil
}
