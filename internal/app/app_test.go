package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSafePathRejectsTraversalAndAbsolutePaths(t *testing.T) {
	base := t.TempDir()
	a := &App{baseDir: base}
	server := Server{ID: "srv_test", Root: "server/test"}
	if err := os.MkdirAll(filepath.Join(base, "server", "test"), 0755); err != nil {
		t.Fatal(err)
	}

	bad := []string{"../config.json", "..\\config.json", "C:/Windows/win.ini", "//share/path", "world/../../../x"}
	for _, raw := range bad {
		if _, _, err := a.safePath(server, raw); err == nil {
			t.Fatalf("safePath(%q) allowed an unsafe path", raw)
		}
	}

	rootTarget, rootRel, err := a.safePath(server, ".")
	if err != nil {
		t.Fatal(err)
	}
	if rootRel != "." || rootTarget != filepath.Join(base, "server", "test") {
		t.Fatalf("unexpected root path: target=%q rel=%q", rootTarget, rootRel)
	}

	target, rel, err := a.safePath(server, "world/level.dat")
	if err != nil {
		t.Fatal(err)
	}
	if rel != filepath.Join("world", "level.dat") {
		t.Fatalf("unexpected rel path: %q", rel)
	}
	if filepath.Dir(target) != filepath.Join(base, "server", "test", "world") {
		t.Fatalf("unexpected target: %q", target)
	}
}

func TestJSONStoreCreatesAndUpdatesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "servers.json")
	store := NewJSONStore(path, []Server{})

	value, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if len(value) != 0 {
		t.Fatalf("expected empty default, got %d", len(value))
	}

	err = store.Update(func(servers []Server) ([]Server, error) {
		return append(servers, Server{ID: "srv_1", Name: "Survival", CreatedAt: time.Now()}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err = store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if len(value) != 1 || value[0].ID != "srv_1" {
		t.Fatalf("unexpected stored value: %#v", value)
	}
}
