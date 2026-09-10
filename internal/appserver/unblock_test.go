package appserver

import "testing"

// The real message from a live run. The phrase is arbitrary — it names a
// specific PR and a specific action — so it can only be read, never guessed.
func TestPhraseIsReadOutOfTheRealMessage(t *testing.T) {
	msg := "Цель заблокирована после трёх повторных попыток: PR #1030 проверен " +
		"(`26/26 success`, `MERGEABLE`), но система безопасности требует явного " +
		"разрешения на merge `main` и удаление ветки.\n\n" +
		"Для возобновления напишите: «мержи PR #1030 и удаляй ветку».\n"

	got := UnblockPhrase(msg)
	if got != "мержи PR #1030 и удаляй ветку" {
		t.Errorf("фраза прочитана неверно: %q", got)
	}
	if !AsksForConfirmation(msg) {
		t.Error("сообщение не опознано как ожидание человека")
	}
}

func TestPhraseInOtherShapes(t *testing.T) {
	cases := map[string]string{
		`Подтвердите: "разрешаю пуш в unxed/f4"`:            "разрешаю пуш в unxed/f4",
		"Напишите «да, продолжай» чтобы продолжить":         "да, продолжай",
		`Please reply with "approve deployment" to proceed`: "approve deployment",
	}
	for msg, want := range cases {
		if got := UnblockPhrase(msg); got != want {
			t.Errorf("%q → %q, ожидалось %q", msg, got, want)
		}
	}
}

// A finished goal is not a waiting one, and inventing a phrase for it would be
// answering a question nobody asked.
func TestNoPhraseWhenNoneIsQuoted(t *testing.T) {
	for _, msg := range []string{
		"Готово: PR #1045 влит, ветка удалена.",
		"Цель заблокирована: не хватает прав.",
		"",
	} {
		if got := UnblockPhrase(msg); got != "" {
			t.Errorf("из %q извлечена выдуманная фраза %q", msg, got)
		}
	}
}

// The same request is worded three ways, and matching only the imperative
// missed two of them — a goal saying «требуется ваше подтверждение» read as a
// goal that had simply finished.
func TestAllWordingsCountAsWaiting(t *testing.T) {
	waiting := []string{
		"Для возобновления напишите: «да»",
		"Цель заблокирована: требуется ваше подтверждение на merge.",
		"Система безопасности требует явного разрешения на merge main.",
		"Reply with \"approve\" to continue",
	}
	for _, msg := range waiting {
		if !AsksForConfirmation(msg) {
			t.Errorf("не опознано как ожидание: %q", msg)
		}
	}

	done := []string{
		"Готово: PR #1045 влит, ветка удалена.",
		"CI зелёный, задача закрыта.",
	}
	for _, msg := range done {
		if AsksForConfirmation(msg) {
			t.Errorf("завершённая цель принята за ожидающую: %q", msg)
		}
	}
}

// crescent's own restart makes the model answer, so the newest message is that
// answer — «Продолжаю текущую цель с того же места…» — while the explanation of
// the block sits a few messages behind it. Reading only the newest found the
// reply and concluded there was nothing to confirm.
func TestRequestIsFoundBeneathTheModelsReply(t *testing.T) {
	newestFirst := []string{
		"Продолжаю текущую цель с того же места: проверяю состояние уже запущенного CI.",
		"",
		"Цель заблокирована. Для возобновления напишите: «мержи PR #1030 и удаляй ветку».",
		"Начинаю работу над целью.",
	}

	var found string
	for _, m := range newestFirst {
		if AsksForConfirmation(m) {
			found = m
			break
		}
	}
	if found == "" {
		t.Fatal("просьба не найдена среди недавних сообщений")
	}
	if got := UnblockPhrase(found); got != "мержи PR #1030 и удаляй ветку" {
		t.Errorf("фраза извлечена неверно: %q", got)
	}
}
