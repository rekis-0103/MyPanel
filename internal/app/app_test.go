package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testApp(t *testing.T) *App {
	t.Helper()
	base := t.TempDir()
	a := &App{
		baseDir:   base,
		dataDir:   filepath.Join(base, "data"),
		config:    NewJSONStore(filepath.Join(base, "data", "config.json"), Config{JWTSecret: "test-secret"}),
		users:     NewJSONStore(filepath.Join(base, "data", "users.json"), []User{}),
		servers:   NewJSONStore(filepath.Join(base, "data", "servers.json"), []Server{}),
		audit:     NewJSONStore(filepath.Join(base, "data", "audit.json"), []AuditEntry{}),
		backups:   NewJSONStore(filepath.Join(base, "data", "backups.json"), []BackupEntry{}),
		console:   NewConsoleHub(consoleMaxBytes),
		processes: NewProcessManager(nil),
	}
	a.processes.app = a
	if err := os.MkdirAll(filepath.Join(base, "server"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := a.config.Set(Config{JWTSecret: "test-secret"}); err != nil {
		t.Fatal(err)
	}
	return a
}

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

func TestRuntimeLifecycleMarkers(t *testing.T) {
	if !detectMinecraftReady(`[11:52:01 INFO]: Done (12.345s)! For help, type "help"`) {
		t.Fatal("expected Done line to mark server ready")
	}
	if detectMinecraftReady(`[11:52:01 INFO]: Done saving chunks`) {
		t.Fatal("unexpected ready detection for unrelated Done line")
	}
	if !detectMinecraftStopping(`[11:52:32 INFO]: [MoonriseCommon] Awaiting termination of worker pool for up to 60s...`) {
		t.Fatal("expected Moonrise shutdown line to mark stopping")
	}

	rt := NewRuntime(&exec.Cmd{}, nil)
	if rt.State() != runtimeStarting {
		t.Fatalf("expected new runtime to start as starting, got %q", rt.State())
	}
	if !rt.MarkRunning() {
		t.Fatal("expected starting runtime to transition to running")
	}
	if rt.State() != runtimeRunning {
		t.Fatalf("expected running state, got %q", rt.State())
	}
	if rt.MarkRunning() {
		t.Fatal("expected duplicate running marker to be ignored")
	}
	rt.MarkStopping()
	if rt.State() != runtimeStopping {
		t.Fatalf("expected stopping state, got %q", rt.State())
	}
	if rt.MarkRunning() {
		t.Fatal("expected running marker to be ignored after stopping")
	}
	rt.SetState(runtimeStopped)
	if rt.State() != runtimeStopped {
		t.Fatalf("expected stopped state, got %q", rt.State())
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

func TestHardcodedAdminLoginWithoutStoredUsers(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"endmin123"}`))
	rr := httptest.NewRecorder()

	a.handleLogin(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected hardcoded admin login, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		User User `json:"user"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.User.Username != "admin" || body.User.Role != "admin" {
		t.Fatalf("unexpected login user: %#v", body.User)
	}
}

func TestHardcodedAdminRejectsWrongPassword(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"wrongpass"}`))
	rr := httptest.NewRecorder()

	a.handleLogin(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong hardcoded admin password to be rejected, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSetupCreatesUserRole(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(`{"username":"player","password":"password123"}`))
	rr := httptest.NewRecorder()

	a.handleSetup(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected setup success, got %d: %s", rr.Code, rr.Body.String())
	}
	users, err := a.users.Get()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Role != "user" {
		t.Fatalf("expected setup user role, got %#v", users)
	}
}

func TestAuditListReturnsLatestFirst(t *testing.T) {
	a := testApp(t)
	user := User{ID: "usr_test", Username: "player", Role: "user"}
	a.addAudit(httptest.NewRequest(http.MethodPost, "/one", nil), user, "first", "server", "srv", "Survival", http.StatusOK, nil)
	a.addAudit(httptest.NewRequest(http.MethodPost, "/two", nil), hardcodedAdmin(), "second", "server", "srv", "Survival", http.StatusOK, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=1", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUser{}, user))
	rr := httptest.NewRecorder()

	a.handleAudit(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected audit list, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Entries []AuditEntry `json:"entries"`
		Total   int          `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 2 || len(body.Entries) != 1 || body.Entries[0].Action != "second" {
		t.Fatalf("unexpected audit response: %#v", body)
	}
}

func TestUserCannotAccessAdminEndpoint(t *testing.T) {
	a := testApp(t)
	user := User{ID: "usr_user", Username: "player", Role: "user", CreatedAt: time.Now()}
	if err := a.users.Set([]User{user}); err != nil {
		t.Fatal(err)
	}
	jwt, err := a.issueJWT(user)
	if err != nil {
		t.Fatal(err)
	}
	userReq := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	userReq.Header.Set("Authorization", "Bearer "+jwt)
	userRR := httptest.NewRecorder()

	a.auth(a.handleUsers, "admin")(userRR, userReq)

	if userRR.Code != http.StatusForbidden {
		t.Fatalf("expected user admin endpoint access to be forbidden, got %d: %s", userRR.Code, userRR.Body.String())
	}
}

func TestStoredAdminRoleIsDowngradedToUser(t *testing.T) {
	a := testApp(t)
	legacy := User{ID: "usr_legacy", Username: "legacy", PasswordHash: hardcodedAdminPasswordHash, Role: "admin", CreatedAt: time.Now()}
	if err := a.users.Set([]User{legacy}); err != nil {
		t.Fatal(err)
	}
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"legacy","password":"endmin123"}`))
	loginRR := httptest.NewRecorder()
	a.handleLogin(loginRR, loginReq)
	if loginRR.Code != http.StatusOK {
		t.Fatalf("expected legacy user login, got %d: %s", loginRR.Code, loginRR.Body.String())
	}
	var body struct {
		Token string `json:"token"`
		User  User   `json:"user"`
	}
	if err := json.NewDecoder(loginRR.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.User.Role != "user" {
		t.Fatalf("expected stored admin role to be downgraded to user, got %#v", body.User)
	}
	adminReq := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	adminReq.Header.Set("Authorization", "Bearer "+body.Token)
	adminRR := httptest.NewRecorder()
	a.auth(a.handleUsers, "admin")(adminRR, adminReq)
	if adminRR.Code != http.StatusForbidden {
		t.Fatalf("expected downgraded stored admin to be forbidden, got %d: %s", adminRR.Code, adminRR.Body.String())
	}
}

func TestTokenRouteRemoved(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil)
	rr := httptest.NewRecorder()

	a.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected API token route to be removed, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestBackupCreateRejectsRunningServer(t *testing.T) {
	a := testApp(t)
	server := Server{ID: "srv_test", Name: "Survival", Slug: "survival", Root: "server/survival"}
	if err := os.MkdirAll(filepath.Join(a.baseDir, "server", "survival"), 0755); err != nil {
		t.Fatal(err)
	}
	a.processes.runtimes[server.ID] = &Runtime{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers/srv_test/backups", strings.NewReader(`{}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxUser{}, User{Role: "user"}))
	rr := httptest.NewRecorder()

	a.handleBackups(rr, req, server, nil)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected backup conflict while running, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleServerRejectsStartupPatchWhileRunning(t *testing.T) {
	store := NewJSONStore(filepath.Join(t.TempDir(), "servers.json"), []Server{})
	server := Server{ID: "srv_test", Name: "Survival", JavaPath: "java", Jar: "server.jar", JVMArgs: []string{"-Xms2G", "-Xmx2G"}, MCArgs: []string{"nogui"}}
	if err := store.Update(func(_ []Server) ([]Server, error) {
		return []Server{server}, nil
	}); err != nil {
		t.Fatal(err)
	}

	a := &App{servers: store, processes: NewProcessManager(nil)}
	a.processes.runtimes[server.ID] = &Runtime{}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/servers/srv_test", strings.NewReader(`{"jar":"paper.jar"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxUser{}, User{Role: "operator"}))
	rr := httptest.NewRecorder()

	a.handleServer(rr, req, server)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected conflict while running, got %d: %s", rr.Code, rr.Body.String())
	}
	servers, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if servers[0].Jar != "server.jar" {
		t.Fatalf("startup config changed while running: %#v", servers[0])
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
