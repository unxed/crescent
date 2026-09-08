package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// EnvCodex overrides the search entirely.
const EnvCodex = "CRESCENT_CODEX"

// System-wide locations, in variables rather than literals so that a test can
// empty them. Without that, a test asserting "nothing is found in an empty
// environment" would pass here and fail on any machine that actually has the
// desktop app installed — which is every machine that matters.
var (
	systemCandidates = []string{
		"/usr/local/bin/codex",
		"/opt/codex/bin/codex",
		"/usr/lib/node_modules/@openai/codex/bin/codex.js",
		"/snap/bin/codex",
	}
	systemAppRoots = []string{
		"/usr/lib/chatgpt",
		"/usr/share/chatgpt",
		"/opt/chatgpt",
		"/usr/lib/codex",
	}
)

// FindCodex locates the Codex CLI and reports every place it looked.
//
// PATH alone is not enough: on a machine with 331 rollout files and daily Codex
// use, `codex` was still absent from PATH. The installers put it in a user-local
// bin, inside an npm or nvm prefix, or in the desktop app's own directory, and
// none of those are on PATH unless the user put them there.
//
// The list of places tried is returned so that a failure can say where it
// looked, instead of only that it failed.
func FindCodex() (path string, tried []string) {
	if v := os.Getenv(EnvCodex); v != "" {
		tried = append(tried, v+"  ("+EnvCodex+")")
		if isExec(v) {
			return v, tried
		}
		return "", tried
	}

	name := "codex"
	if runtime.GOOS == "windows" {
		name = "codex.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, append(tried, p+"  (PATH)")
	}
	tried = append(tried, "PATH")

	for _, cand := range candidates() {
		tried = append(tried, cand)
		if isExec(cand) {
			return cand, tried
		}
	}

	// Last resort: walk the desktop app's own installation. The ChatGPT app
	// ships the Codex CLI inside itself, and the layout is neither documented
	// nor stable — on Windows it sits under a per-version directory. Rather
	// than guess the shape, look for the binary.
	for _, root := range appRoots() {
		tried = append(tried, root+"  (обход)")
		if p := searchUnder(root, name, 7); p != "" {
			return p, tried
		}
	}
	return "", tried
}

// appRoots are installation directories of the desktop app, which bundles the
// CLI. Each is walked only if it exists.
func appRoots() []string {
	home, _ := os.UserHomeDir()
	join := filepath.Join

	switch runtime.GOOS {
	case "windows":
		var out []string
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			out = append(out, join(la, "OpenAI"), join(la, "Programs", "OpenAI"))
		}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			out = append(out, join(pf, "OpenAI"))
		}
		return out
	case "darwin":
		var out []string
		if home != "" {
			out = append(out, join(home, "Library", "Application Support", "OpenAI"))
		}
		return append(out, "/Applications/ChatGPT.app", "/Applications/Codex.app")
	default:
		out := append([]string(nil), systemAppRoots...)
		if home != "" {
			out = append(out,
				join(home, ".local", "share", "OpenAI"),
				join(home, ".local", "share", "chatgpt"),
				join(home, ".local", "state", "chatgpt"))
		}
		return out
	}
}

// searchUnder walks root looking for an executable with the given name. Depth
// is bounded so that pointing this at a large tree cannot turn into a scan of
// the whole disk.
func searchUnder(root string, name string, maxDepth int) string {
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return ""
	}
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))

	var found string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			if strings.Count(path, string(os.PathSeparator))-rootDepth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == name && isExec(path) {
			found = path
		}
		return nil
	})
	return found
}

// candidates lists the install locations seen in the wild, most likely first.
func candidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	join := func(parts ...string) string { return filepath.Join(parts...) }

	var out []string
	if runtime.GOOS == "windows" {
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			// The desktop app installs into a per-version directory:
			// %LOCALAPPDATA%\OpenAI\Codex\bin\<version>\codex.exe
			out = append(out, newestMatch(join(la, "OpenAI", "Codex", "bin", "*", "codex.exe"))...)
			out = append(out,
				join(la, "OpenAI", "Codex", "bin", "codex.exe"),
				join(la, "Programs", "OpenAI", "Codex", "bin", "codex.exe"),
				join(la, "Programs", "codex", "codex.exe"))
		}
		if ad := os.Getenv("APPDATA"); ad != "" {
			out = append(out, join(ad, "npm", "codex.cmd"), join(ad, "npm", "codex.exe"))
		}
		return out
	}

	if home != "" {
		out = append(out,
			join(home, ".local", "bin", "codex"),
			join(home, ".codex", "bin", "codex"),
			join(home, ".npm-global", "bin", "codex"),
			join(home, "bin", "codex"),
			join(home, ".bun", "bin", "codex"),
		)
		// Versioned directories: newest first.
		out = append(out, newestMatch(join(home, ".local", "share", "OpenAI", "Codex", "bin", "*", "codex"))...)
		out = append(out, newestMatch(join(home, ".nvm", "versions", "node", "*", "bin", "codex"))...)
	}
	out = append(out, systemCandidates...)
	if runtime.GOOS == "darwin" {
		out = append(out,
			"/Applications/Codex.app/Contents/MacOS/codex",
			"/opt/homebrew/bin/codex")
	}
	return out
}

// newestMatch expands a glob and returns the matches newest-looking first, so a
// per-version directory yields the latest build rather than the alphabetically
// first one.
func newestMatch(pattern string) []string {
	matches, _ := filepath.Glob(pattern)
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	return matches
}

func isExec(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode()&0o111 != 0
}
