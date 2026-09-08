package runner

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Doctor is what crescent could learn about the machine it runs on. Every
// assumption the plan makes about the Codex CLI is checked here rather than
// discovered when a real turn is spent.
type Doctor struct {
	CodexPath    string
	CodexVersion string
	Tried        []string // every location searched, for when nothing is found
	HasExec      bool
	HasResume    bool
	HasJSON      bool
	Problems     []string
}

// Diagnose looks for the codex binary and asks it what it supports.
//
// This exists because crescent builds a command line against an undocumented,
// moving CLI: `codex exec resume` was not in released builds when it first
// appeared in the source tree. Finding out from --help is cheap; finding out
// from a failed unattended run at 4am is not.
func Diagnose() Doctor {
	var d Doctor

	path, tried := FindCodex()
	d.Tried = tried
	if path == "" {
		d.Problems = append(d.Problems,
			"исполняемый файл codex не найден; укажите его через "+EnvCodex+" или -codex")
		return d
	}
	d.CodexPath = path

	if out, err := run(path, "--version"); err == nil {
		d.CodexVersion = strings.TrimSpace(firstLine(out))
	} else {
		d.Problems = append(d.Problems, "codex --version не отработал: "+err.Error())
	}

	help, err := run(path, "exec", "--help")
	if err != nil {
		d.Problems = append(d.Problems, "codex exec --help не отработал: "+err.Error())
		return d
	}
	d.HasExec = true
	d.HasJSON = strings.Contains(help, "--json")
	d.HasResume = strings.Contains(help, "resume")

	if !d.HasJSON {
		d.Problems = append(d.Problems,
			"codex exec не знает --json: следить за ходом сессии будет нечем")
	}
	if !d.HasResume {
		d.Problems = append(d.Problems,
			"codex exec не знает подкоманду resume: продолжить сессию этой сборкой нельзя")
	}
	return d
}

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
