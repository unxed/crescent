package pins

import "testing"

// "Tick the ones you want and forget about it" only works if the ticks outlive
// the process, so pinning must round-trip through disk.
func TestPinsPersist(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	p := Load()
	if p.Count() != 0 {
		t.Fatal("новый набор не пуст")
	}
	p.Set("t1", "Konsole", true)
	p.Set("t2", "Лунобот-1", true)
	p.Set("t2", "Лунобот-1", false)

	again := Load()
	if again.Count() != 1 || !again.Pinned("t1") || again.Pinned("t2") {
		t.Errorf("после перезагрузки: count=%d t1=%v t2=%v",
			again.Count(), again.Pinned("t1"), again.Pinned("t2"))
	}
	if again.Label("t1") != "Konsole" {
		t.Errorf("метка не сохранилась: %q", again.Label("t1"))
	}
}
