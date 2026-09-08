package main

import (
	"fmt"
	"os"
	"time"

	"github.com/unxed/crescent/archive/internal/codex"
)

// scanProgress reports a slow first sweep, and only that.
//
// Rollouts are now read end to end, because a goal set mid-session lives in the
// middle of the file. On a directory of a few hundred sessions the first pass
// therefore reads gigabytes and takes a while; afterwards the cache makes it
// instant and only growing sessions are re-read. Silence during those first
// seconds would look like a hang, so progress is printed — but on stderr, so it
// never contaminates output someone is piping somewhere.
func scanProgress() codex.Progress {
	started := time.Now()
	announced := false
	last := time.Time{}

	return func(done, total int, path string) {
		if !announced {
			announced = true
			fmt.Fprintf(os.Stderr,
				"Первый проход: читаю сессии целиком (%d файлов). Дальше будет быстро.\n", total)
		}
		if time.Since(last) < 300*time.Millisecond && done != total-1 {
			return
		}
		last = time.Now()
		fmt.Fprintf(os.Stderr, "\r  %d/%d, %s   ", done+1, total, time.Since(started).Round(time.Second))
	}
}

// finishProgress clears the progress line if one was printed.
func finishProgress() {
	fmt.Fprint(os.Stderr, "\r                                                            \r")
}
