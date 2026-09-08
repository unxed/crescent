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
func runFind(dir, needle string, deep bool) error {
	where := "в той части файлов, которую читает сканер"
	if deep {
		where = "во всех файлах целиком (может быть долго)"
	}
	fmt.Printf("Ищу %q %s\n\n", needle, where)

	hits, err := codex.Find(dir, needle, deep, 200)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("Не найдено.")
		if !deep {
			fmt.Println("Попробуйте с -deep: значение может лежать в середине файла,")
			fmt.Println("куда сканер не заглядывает.")
		}
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
		mark := ""
		if !hs[0].InScan {
			mark = "   ← вне области, которую читает сканер"
		}
		fmt.Printf("\n  %s   (совпадений: %d)%s\n", k, len(hs), mark)
		fmt.Printf("      значение: %s\n", hs[0].Value)
		fmt.Printf("      файл:     %s, строка %d\n", filepath.Base(hs[0].Path), hs[0].Line)
	}
	return nil
}
