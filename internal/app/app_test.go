package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

func TestConsoleWebSocketUpgradesThroughStatusWriter(t *testing.T) {
	a := &App{
		console:   NewConsoleHub(consoleMaxBytes),
		processes: NewProcessManager(nil),
		updater: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	server := Server{ID: "srv_test"}
	a.console.Append(server.ID, "[mypanel] previous shutdown log\r\n")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := &statusWriter{ResponseWriter: w, status: 200}
		a.handleConsoleWS(ww, r, server)
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(msg), "previous shutdown log") {
		t.Fatalf("unexpected console message: %q", msg)
	}
}

func TestConsoleHubKeepsHistoryAfterSubscriberLeaves(t *testing.T) {
	hub := NewConsoleHub(1024)
	ch := hub.Subscribe("srv_test")
	hub.Append("srv_test", "server booting\r\n")
	hub.Unsubscribe("srv_test", ch)

	snapshot := strings.Join(hub.Snapshot("srv_test"), "")
	if !strings.Contains(snapshot, "server booting") {
		t.Fatalf("expected retained console history, got %q", snapshot)
	}
}

func TestConsoleHubTrimsByByteLimit(t *testing.T) {
	hub := NewConsoleHub(10)
	hub.Append("srv_test", "12345")
	hub.Append("srv_test", "67890")
	hub.Append("srv_test", "abc")

	snapshot := strings.Join(hub.Snapshot("srv_test"), "")
	if snapshot != "67890abc" {
		t.Fatalf("unexpected trimmed console history: %q", snapshot)
	}

	hub.Append("srv_test", "abcdefghijklmnopqrstuvwxyz")
	snapshot = strings.Join(hub.Snapshot("srv_test"), "")
	if snapshot != "qrstuvwxyz" {
		t.Fatalf("unexpected oversized message trim: %q", snapshot)
	}
}

func TestSameVersionIgnoresLeadingV(t *testing.T) {
	if !sameVersion("v1.2.3", "1.2.3") {
		t.Fatal("expected versions with leading v to match")
	}
	if sameVersion("v1.2.4", "1.2.3") {
		t.Fatal("expected different versions not to match")
	}
}

func TestMetricSamplerCalculatesRawMulticoreCPU(t *testing.T) {
	sampler := NewMetricSampler()
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	if got := sampler.SampleValue("srv_test", 100, 200, base, 10); got != 0 {
		t.Fatalf("first sample should warm up, got %.2f", got)
	}
	if got := sampler.SampleValue("srv_test", 100, 200, base.Add(2*time.Second), 15); got != 250 {
		t.Fatalf("expected raw multicore CPU 250, got %.2f", got)
	}
}

func TestMetricSamplerResetsWhenProcessChanges(t *testing.T) {
	sampler := NewMetricSampler()
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	sampler.SampleValue("srv_test", 100, 200, base, 10)

	if got := sampler.SampleValue("srv_test", 101, 300, base.Add(time.Second), 30); got != 0 {
		t.Fatalf("expected process change to reset baseline, got %.2f", got)
	}
	if got := sampler.SampleValue("srv_test", 101, 300, base.Add(2*time.Second), 31); got != 100 {
		t.Fatalf("expected new process delta after reset, got %.2f", got)
	}
}

func TestMetricSamplerRejectsInvalidDelta(t *testing.T) {
	sampler := NewMetricSampler()
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	sampler.SampleValue("srv_test", 100, 200, base, 10)

	if got := sampler.SampleValue("srv_test", 100, 200, base, 11); got != 0 {
		t.Fatalf("expected zero for non-positive wall delta, got %.2f", got)
	}
	if got := sampler.SampleValue("srv_test", 100, 200, base.Add(time.Second), 9); got != 0 {
		t.Fatalf("expected zero for negative CPU delta, got %.2f", got)
	}
}
