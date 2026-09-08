// Package runner decides what crescent would do, and builds the commands to do
// it. Nothing here executes anything: the planning is separated from the doing
// so that a plan can be printed and checked before a single Codex turn is spent.
package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/unxed/crescent/archive/internal/codex"
)

// Policy is what the user is willing to let an unattended agent do.
type Policy struct {
	// Sandbox is the Codex sandbox mode. workspace-write lets the agent edit
	// its own checkout and nothing else.
	Sandbox string

	// WritableRoots are paths outside the workspace the agent may still write
	// to. The Go build and module caches belong here: without them every
	// resumed build re-downloads the world, and with a read-only cache it
	// simply fails.
	WritableRoots []string

	// Prompt is the message sent to wake a session.
	Prompt string

	// QuietPeriod is how long a session must have been untouched before
	// crescent will drive it. A rollout written seconds ago means a human is
	// in that thread right now.
	QuietPeriod time.Duration

	// MaxTurns caps consecutive turns per session before moving to the next,
	// so one goal cannot monopolise the whole window.
	MaxTurns int

	// CodexPath is the binary to invoke. Resolved rather than assumed, because
	// the CLI is routinely installed somewhere that is not on PATH.
	CodexPath string

	// Disabled holds session ids the user has taken out of the queue. Kept as
	// an opt-out rather than an opt-in so that a goal created after the window
	// was opened still gets picked up.
	Disabled map[string]bool
}

// DefaultPolicy is deliberately conservative: it can edit its own workspace and
// warm the Go caches, and nothing else.
func DefaultPolicy() Policy {
	path, _ := FindCodex()
	if path == "" {
		path = "codex"
	}
	return Policy{
		CodexPath:     path,
		Sandbox:       "workspace-write",
		WritableRoots: GoCaches(),
		Prompt:        "Продолжай работу над текущей целью.",
		QuietPeriod:   15 * time.Minute,
		MaxTurns:      1,
	}
}

// GoCaches reports the Go module and build caches, which live outside any
// workspace and must stay writable for a build to work at all.
//
// Asked of the Go toolchain rather than guessed. The guess was wrong on
// Windows in two ways at once — it joined paths with forward slashes and used
// the Unix layout, producing "C:\\users\\me/.cache/go-build", which is neither
// a real path nor where Windows keeps the build cache. Running under Wine is
// what showed it.
func GoCaches() []string {
	if v := os.Getenv("GOMODCACHE"); v != "" {
		if b := os.Getenv("GOCACHE"); b != "" {
			return []string{v, b}
		}
	}
	if out, err := goEnv("GOMODCACHE", "GOCACHE"); err == nil && len(out) > 0 {
		return out
	}
	return defaultGoCaches()
}

// goEnv asks the toolchain where its caches are. This is authoritative on every
// platform and costs one process.
func goEnv(vars ...string) ([]string, error) {
	bin, err := exec.LookPath("go")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, append([]string{"env"}, vars...)...).Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// defaultGoCaches is the last resort, following each platform's own layout
// instead of one platform's.
func defaultGoCaches() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	modCache := filepath.Join(home, "go", "pkg", "mod")

	if runtime.GOOS == "windows" {
		// The build cache lives under LOCALAPPDATA here, not under a dot
		// directory in the home folder.
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		return []string{modCache, filepath.Join(base, "go-build")}
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		cache = filepath.Join(home, ".cache")
	}
	return []string{modCache, filepath.Join(cache, "go-build")}
}

// SkipReason explains why a session is not in the queue. Empty means it is.
type SkipReason string

// Why a goal session may be skipped.
const (
	SkipNotResumable SkipReason = "цель завершена или заблокирована"
	SkipNoID         SkipReason = "не удалось определить id сессии"
	SkipNoCwd        SkipReason = "рабочий каталог не найден"
	SkipBusy         SkipReason = "сессия только что менялась — похоже, в ней работают"
	SkipDisabled     SkipReason = "снята галочкой"
)

// Step is one planned resume.
type Step struct {
	Session codex.Session
	Args    []string   // argv for the codex binary, without the binary itself
	Skip    SkipReason // non-empty: this session is not going to run
}

// Plan is the whole decision: what runs, in what order, and when it can start.
type Plan struct {
	Steps      []Step
	StartAt    time.Time // zero means "right now"
	Limited    bool      // the account is inside an exhausted window
	Interrupt  bool      // a human appears to be working; crescent stands down
	YieldUntil time.Time // when the quiet period around that work runs out
}

// OwnWrites records when crescent itself last finished a turn for a session,
// so its own footprints are not mistaken for someone else's.
type OwnWrites map[string]time.Time

// ownSlack is how long after a turn a rollout may still be settling. The file
// is written while the turn runs, and the modification time lands somewhere
// around its end.
const ownSlack = 60 * time.Second

// touchedByUs reports whether a session's latest change looks like crescent's
// own work rather than a person's.
func (o OwnWrites) touchedByUs(s codex.Session) bool {
	at, ok := o[s.ID]
	return ok && s.Modified.Before(at.Add(ownSlack))
}

// Build works out what crescent would do with the sessions it found.
//
// Two rules shape the result and are worth stating plainly. Goals run one at a
// time, because the usage limit belongs to the account: three goals in parallel
// would empty a five-hour window in twenty minutes and, worse, could have two
// agents editing one checkout. And crescent yields to a human: if any rollout
// in the directory changed within the quiet period, someone is at the keyboard
// and the whole plan waits.
func Build(sessions []codex.Session, p Policy, now time.Time, own OwnWrites) Plan {
	var plan Plan

	// A rollout touched a moment ago means a human is in that thread — unless
	// crescent is the one who touched it. Without this exception the daemon
	// reads its own turn as someone else's work and stands down for the whole
	// quiet period, managing one turn every fifteen minutes.
	for _, s := range sessions {
		if now.Sub(s.Modified) >= p.QuietPeriod || own.touchedByUs(s) {
			continue
		}
		plan.Interrupt = true
		plan.YieldUntil = s.Modified.Add(p.QuietPeriod)
		break
	}

	if next := codex.NextReset(sessions); !next.IsZero() {
		plan.Limited = true
		plan.StartAt = next
	}

	var goals []codex.Session
	for _, s := range sessions {
		if s.HasGoal() {
			goals = append(goals, s)
		}
	}
	// Freshest first: the goal you touched most recently is the one you care
	// about most.
	sort.SliceStable(goals, func(i, j int) bool { return goals[i].Modified.After(goals[j].Modified) })

	for _, s := range goals {
		step := Step{Session: s}
		switch {
		case p.Disabled[s.ID]:
			step.Skip = SkipDisabled
		case !s.Status.Resumable():
			step.Skip = SkipNotResumable
		case s.ID == "":
			step.Skip = SkipNoID
		case s.Cwd == "":
			step.Skip = SkipNoCwd
		case !dirExists(s.Cwd):
			// Many sessions run out of /tmp, which does not survive a reboot.
			step.Skip = SkipNoCwd
		case now.Sub(s.Modified) < p.QuietPeriod && !own.touchedByUs(s):
			step.Skip = SkipBusy
		default:
			step.Args = BuildArgs(s, p)
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan
}

// Runnable returns only the steps that would actually execute.
func (p Plan) Runnable() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Skip == "" {
			out = append(out, s)
		}
	}
	return out
}

// BuildArgs assembles the argv for resuming one session.
//
// Flag order matters: --json and the sandbox settings belong to `codex exec`
// and must precede the `resume` subcommand, while the session id and the prompt
// are arguments of `resume`.
func BuildArgs(s codex.Session, p Policy) []string {
	args := []string{"exec", "--json", "--cd", s.Cwd}
	if p.Sandbox != "" {
		args = append(args, "--sandbox", p.Sandbox)
	}
	if len(p.WritableRoots) > 0 {
		args = append(args, "-c", "sandbox_workspace_write.writable_roots="+tomlList(p.WritableRoots))
	}
	args = append(args, "resume", s.ID, p.Prompt)
	return args
}

// tomlList renders a string slice the way Codex's -c overrides expect it.
func tomlList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
