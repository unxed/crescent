package pins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Pins is the set of goals the user has chosen to keep alive: the checkboxes in
// the window, remembered across runs. "Tick the ones you want, close the
// window, forget about it" only works if the ticks outlive the process.
//
// A goal is stored by its thread id, which is stable, together with the label
// it had when pinned — enough to show the choice even before the server has
// been asked, and enough to explain a pin whose thread has since gone.
type Pins struct {
	path string

	mu    sync.Mutex
	items map[string]string // threadID -> label
}

// Path is the file the pins live in.
func Path() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "crescent", "pins.json"), nil
}

// Load reads the saved pins, or returns an empty set if there are none.
func Load() *Pins {
	p := &Pins{items: map[string]string{}}
	path, err := Path()
	if err != nil {
		return p
	}
	p.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var stored map[string]string
	if json.Unmarshal(data, &stored) == nil && stored != nil {
		p.items = stored
	}
	return p
}

// Pinned reports whether a thread is pinned.
func (p *Pins) Pinned(threadID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.items[threadID]
	return ok
}

// Set pins or unpins a thread, and saves. Saving on every change is what makes
// the window's checkboxes durable without a separate "save" step.
func (p *Pins) Set(threadID, label string, pinned bool) {
	p.mu.Lock()
	if pinned {
		p.items[threadID] = label
	} else {
		delete(p.items, threadID)
	}
	p.mu.Unlock()
	p.save()
}

// IDs returns the pinned thread ids, sorted for a stable order.
func (p *Pins) IDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.items))
	for id := range p.items {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Label returns the remembered label of a pinned thread.
func (p *Pins) Label(threadID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.items[threadID]
}

// Count is how many goals are pinned.
func (p *Pins) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

func (p *Pins) save() {
	if p.path == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(p.path), 0o755) != nil {
		return
	}
	p.mu.Lock()
	data, err := json.MarshalIndent(p.items, "", "  ")
	p.mu.Unlock()
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, p.path)
	}
}
