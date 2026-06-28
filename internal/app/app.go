package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/crypto/bcrypt"

	"mypanel/internal/static"
)

type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

type Config struct {
	ListenAddr          string `json:"listenAddr"`
	GitHubRepo          string `json:"githubRepo"`
	UpdateCheckInterval int    `json:"updateCheckIntervalMinutes"`
	JWTSecret           string `json:"jwtSecret"`
}

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
}

type Server struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Root      string    `json:"root"`
	JavaPath  string    `json:"javaPath"`
	Jar       string    `json:"jar"`
	JVMArgs   []string  `json:"jvmArgs"`
	MCArgs    []string  `json:"mcArgs"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type App struct {
	build      BuildInfo
	baseDir    string
	dataDir    string
	serverRoot string
	config     *JSONStore[Config]
	users      *JSONStore[[]User]
	servers    *JSONStore[[]Server]
	processes  *ProcessManager
	updater    websocket.Upgrader
	http       *http.ServeMux
	client     *http.Client
}

func Run(build BuildInfo) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	baseDir := filepath.Dir(exe)
	if strings.HasSuffix(strings.ToLower(exe), ".test.exe") || strings.Contains(strings.ToLower(exe), `\go-build`) {
		if wd, err := os.Getwd(); err == nil {
			baseDir = wd
		}
	}

	a := &App{
		build:      build,
		baseDir:    baseDir,
		dataDir:    filepath.Join(baseDir, "data"),
		serverRoot: filepath.Join(baseDir, "server"),
		updater: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				u, err := http.NewRequest(http.MethodGet, origin, nil)
				if err != nil {
					return false
				}
				return strings.EqualFold(u.URL.Host, r.Host)
			},
		},
		client: &http.Client{Timeout: 45 * time.Second},
	}
	if err := a.init(); err != nil {
		return err
	}
	log.Printf("MyPanel %s listening on http://%s", build.Version, a.mustConfig().ListenAddr)
	return http.ListenAndServe(a.mustConfig().ListenAddr, a.routes())
}

func (a *App) init() error {
	for _, dir := range []string{a.dataDir, a.serverRoot, filepath.Join(a.dataDir, "logs"), filepath.Join(a.dataDir, "updates")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	a.config = NewJSONStore(filepath.Join(a.dataDir, "config.json"), Config{
		ListenAddr:          "127.0.0.1:8080",
		GitHubRepo:          "",
		UpdateCheckInterval: 60,
		JWTSecret:           randomHex(32),
	})
	a.users = NewJSONStore(filepath.Join(a.dataDir, "users.json"), []User{})
	a.servers = NewJSONStore(filepath.Join(a.dataDir, "servers.json"), []Server{})
	if _, err := a.config.Get(); err != nil {
		return err
	}
	if _, err := a.users.Get(); err != nil {
		return err
	}
	if _, err := a.servers.Get(); err != nil {
		return err
	}
	a.processes = NewProcessManager(a)
	return nil
}

func (a *App) mustConfig() Config {
	cfg, err := a.config.Get()
	if err != nil {
		panic(err)
	}
	return cfg
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", a.handleHealth)
	mux.HandleFunc("/api/v1/setup", a.handleSetup)
	mux.HandleFunc("/api/v1/auth/login", a.handleLogin)
	mux.HandleFunc("/api/v1/me", a.auth(a.handleMe, "viewer"))
	mux.HandleFunc("/api/v1/servers", a.auth(a.handleServers, "viewer"))
	mux.HandleFunc("/api/v1/servers/", a.auth(a.handleServerResource, "viewer"))
	mux.HandleFunc("/api/v1/users", a.auth(a.handleUsers, "admin"))
	mux.HandleFunc("/api/v1/config", a.auth(a.handleConfig, "admin"))
	mux.HandleFunc("/api/v1/update/status", a.auth(a.handleUpdateStatus, "admin"))
	mux.HandleFunc("/api/v1/update/apply", a.auth(a.handleUpdateApply, "admin"))
	mux.HandleFunc("/", a.handleStatic)
	return a.withLog(mux)
}

func (a *App) withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := randomHex(8)
		ctx := context.WithValue(r.Context(), ctxRequestID{}, rid)
		ww := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r.WithContext(ctx))
		log.Printf("%s %s %d %s rid=%s", r.Method, r.URL.Path, ww.status, time.Since(start), rid)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

type ctxRequestID struct{}
type ctxUser struct{}

func (a *App) auth(next http.HandlerFunc, minRole string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" && r.URL.Query().Get("token") != "" {
			auth = "Bearer " + r.URL.Query().Get("token")
		}
		if !strings.HasPrefix(auth, "Bearer ") {
			writeErr(w, r, http.StatusUnauthorized, "unauthorized", "Login required")
			return
		}
		claims := jwt.MapClaims{}
		token, err := jwt.ParseWithClaims(strings.TrimPrefix(auth, "Bearer "), claims, func(token *jwt.Token) (any, error) {
			if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, errors.New("unexpected signing method")
			}
			return []byte(a.mustConfig().JWTSecret), nil
		})
		if err != nil || !token.Valid {
			writeErr(w, r, http.StatusUnauthorized, "unauthorized", "Invalid token")
			return
		}
		userID, _ := claims["sub"].(string)
		user, ok := a.findUserByID(userID)
		if !ok || !roleAllows(user.Role, minRole) {
			writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, user)))
	}
}

func roleAllows(role, min string) bool {
	ranks := map[string]int{"viewer": 1, "operator": 2, "admin": 3}
	return ranks[role] >= ranks[min]
}

func currentUser(r *http.Request) User {
	user, _ := r.Context().Value(ctxUser{}).(User)
	return user
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": a.build,
		"baseDir": a.baseDir,
	})
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST")
		return
	}
	users, err := a.users.Get()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if len(users) > 0 {
		writeErr(w, r, http.StatusConflict, "setup_done", "Initial admin already exists")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Username) < 3 || len(req.Password) < 8 {
		writeErr(w, r, http.StatusBadRequest, "invalid_setup", "Username must be at least 3 characters and password at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "hash_error", err.Error())
		return
	}
	user := User{ID: newID("usr"), Username: req.Username, PasswordHash: string(hash), Role: "admin", CreatedAt: time.Now()}
	if err := a.users.Set([]User{user}); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, publicUser(user))
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	users, err := a.users.Get()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	for _, user := range users {
		if strings.EqualFold(user.Username, req.Username) && bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) == nil {
			token, err := a.issueJWT(user)
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, "token_error", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": publicUser(user)})
			return
		}
	}
	writeErr(w, r, http.StatusUnauthorized, "bad_credentials", "Invalid username or password")
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, publicUser(currentUser(r)))
}

func (a *App) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users, err := a.users.Get()
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		out := make([]map[string]any, 0, len(users))
		for _, user := range users {
			out = append(out, publicUser(user))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Role == "" {
			req.Role = "viewer"
		}
		if !roleAllows(req.Role, "viewer") || len(req.Username) < 3 || len(req.Password) < 8 {
			writeErr(w, r, http.StatusBadRequest, "invalid_user", "Invalid username, password, or role")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "hash_error", err.Error())
			return
		}
		var created User
		err = a.users.Update(func(users []User) ([]User, error) {
			for _, user := range users {
				if strings.EqualFold(user.Username, req.Username) {
					return users, errors.New("username already exists")
				}
			}
			created = User{ID: newID("usr"), Username: req.Username, PasswordHash: string(hash), Role: req.Role, CreatedAt: time.Now()}
			return append(users, created), nil
		})
		if err != nil {
			writeErr(w, r, http.StatusConflict, "user_exists", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, publicUser(created))
	default:
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
	}
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := a.mustConfig()
		cfg.JWTSecret = ""
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPatch:
		var req struct {
			ListenAddr          *string `json:"listenAddr"`
			GitHubRepo          *string `json:"githubRepo"`
			UpdateCheckInterval *int    `json:"updateCheckIntervalMinutes"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		var out Config
		err := a.config.Update(func(cfg Config) (Config, error) {
			if req.ListenAddr != nil && *req.ListenAddr != "" {
				cfg.ListenAddr = *req.ListenAddr
			}
			if req.GitHubRepo != nil {
				cfg.GitHubRepo = strings.TrimSpace(*req.GitHubRepo)
			}
			if req.UpdateCheckInterval != nil && *req.UpdateCheckInterval > 0 {
				cfg.UpdateCheckInterval = *req.UpdateCheckInterval
			}
			if cfg.JWTSecret == "" {
				cfg.JWTSecret = randomHex(32)
			}
			out = cfg
			return cfg, nil
		})
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		out.JWTSecret = ""
		writeJSON(w, http.StatusOK, out)
	default:
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
	}
}

func (a *App) handleServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		servers, err := a.servers.Get()
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		out := make([]map[string]any, 0, len(servers))
		for _, srv := range servers {
			out = append(out, a.serverDTO(srv))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		if !roleAllows(currentUser(r).Role, "operator") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
			return
		}
		var req struct {
			Name     string   `json:"name"`
			Slug     string   `json:"slug"`
			JavaPath string   `json:"javaPath"`
			Jar      string   `json:"jar"`
			JVMArgs  []string `json:"jvmArgs"`
			MCArgs   []string `json:"mcArgs"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.JavaPath == "" {
			req.JavaPath = "java"
		}
		if req.Jar == "" {
			req.Jar = "server.jar"
		}
		if len(req.MCArgs) == 0 {
			req.MCArgs = []string{"nogui"}
		}
		slug := cleanSlug(req.Slug)
		if slug == "" {
			slug = cleanSlug(req.Name)
		}
		if req.Name == "" || slug == "" {
			writeErr(w, r, http.StatusBadRequest, "invalid_server", "Server name is required")
			return
		}
		root := filepath.Join("server", slug)
		absRoot := filepath.Join(a.baseDir, root)
		var created Server
		err := a.servers.Update(func(servers []Server) ([]Server, error) {
			for _, srv := range servers {
				if srv.Slug == slug {
					return servers, fmt.Errorf("server folder %q already exists", slug)
				}
			}
			now := time.Now()
			created = Server{ID: newID("srv"), Name: req.Name, Slug: slug, Root: filepath.ToSlash(root), JavaPath: req.JavaPath, Jar: req.Jar, JVMArgs: req.JVMArgs, MCArgs: req.MCArgs, CreatedAt: now, UpdatedAt: now}
			return append(servers, created), nil
		})
		if err != nil {
			writeErr(w, r, http.StatusConflict, "server_exists", err.Error())
			return
		}
		if err := os.MkdirAll(absRoot, 0755); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "mkdir_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, a.serverDTO(created))
	default:
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
	}
}

func (a *App) handleServerResource(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/servers/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, r, http.StatusNotFound, "not_found", "Server not found")
		return
	}
	server, ok := a.findServer(parts[0])
	if !ok {
		writeErr(w, r, http.StatusNotFound, "not_found", "Server not found")
		return
	}
	if len(parts) == 1 {
		a.handleServer(w, r, server)
		return
	}
	switch parts[1] {
	case "runtime":
		a.handleRuntime(w, r, server)
	case "console":
		a.handleConsole(w, r, server)
	case "files":
		a.handleFiles(w, r, server)
	case "metrics":
		a.handleMetrics(w, r, server)
	default:
		writeErr(w, r, http.StatusNotFound, "not_found", "Unknown server resource")
	}
}

func (a *App) handleServer(w http.ResponseWriter, r *http.Request, server Server) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, a.serverDTO(server))
	case http.MethodPatch:
		if !roleAllows(currentUser(r).Role, "operator") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
			return
		}
		var req struct {
			Name     *string  `json:"name"`
			JavaPath *string  `json:"javaPath"`
			Jar      *string  `json:"jar"`
			JVMArgs  []string `json:"jvmArgs"`
			MCArgs   []string `json:"mcArgs"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		var updated Server
		err := a.servers.Update(func(servers []Server) ([]Server, error) {
			for i := range servers {
				if servers[i].ID == server.ID {
					if req.Name != nil && *req.Name != "" {
						servers[i].Name = *req.Name
					}
					if req.JavaPath != nil && *req.JavaPath != "" {
						servers[i].JavaPath = *req.JavaPath
					}
					if req.Jar != nil && *req.Jar != "" {
						servers[i].Jar = *req.Jar
					}
					if req.JVMArgs != nil {
						servers[i].JVMArgs = req.JVMArgs
					}
					if req.MCArgs != nil {
						servers[i].MCArgs = req.MCArgs
					}
					servers[i].UpdatedAt = time.Now()
					updated = servers[i]
				}
			}
			return servers, nil
		})
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, a.serverDTO(updated))
	case http.MethodDelete:
		if !roleAllows(currentUser(r).Role, "admin") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
			return
		}
		if a.processes.IsRunning(server.ID) {
			writeErr(w, r, http.StatusConflict, "server_running", "Stop the server before deleting it")
			return
		}
		deleteFiles := r.URL.Query().Get("deleteFiles") == "true"
		err := a.servers.Update(func(servers []Server) ([]Server, error) {
			next := servers[:0]
			for _, srv := range servers {
				if srv.ID != server.ID {
					next = append(next, srv)
				}
			}
			return next, nil
		})
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if deleteFiles {
			_ = os.RemoveAll(a.absServerRoot(server))
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	default:
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
	}
}

func (a *App) handleRuntime(w http.ResponseWriter, r *http.Request, server Server) {
	if !roleAllows(currentUser(r).Role, "operator") {
		writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
		return
	}
	if r.Method != http.MethodPut {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use PUT")
		return
	}
	var req struct {
		State string `json:"state"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var err error
	switch req.State {
	case "running":
		err = a.processes.Start(server)
	case "stopped":
		err = a.processes.Stop(server.ID, 20*time.Second)
	default:
		writeErr(w, r, http.StatusBadRequest, "invalid_state", "State must be running or stopped")
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusConflict, "runtime_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.serverDTO(server))
}

func (a *App) handleConsole(w http.ResponseWriter, r *http.Request, server Server) {
	if !roleAllows(currentUser(r).Role, "operator") {
		writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/ws") {
		a.handleConsoleWS(w, r, server)
		return
	}
	writeErr(w, r, http.StatusNotFound, "not_found", "Console endpoint not found")
}

func (a *App) handleConsoleWS(w http.ResponseWriter, r *http.Request, server Server) {
	conn, err := a.updater.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	rt, ok := a.processes.Get(server.ID)
	if !ok {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\nServer is not running.\r\n"))
		return
	}
	ch := rt.Subscribe()
	defer rt.Unsubscribe(ch)
	for _, line := range rt.Buffer() {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(line))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range ch {
			if conn.WriteMessage(websocket.TextMessage, []byte(msg)) != nil {
				return
			}
		}
	}()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if len(msg) > 0 {
			_ = rt.Write(msg)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func (a *App) handleFiles(w http.ResponseWriter, r *http.Request, server Server) {
	if r.Method != http.MethodGet && !roleAllows(currentUser(r).Role, "operator") {
		writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
		return
	}
	idx := strings.Index(r.URL.Path, "/files")
	action := ""
	if idx >= 0 {
		action = r.URL.Path[idx+len("/files"):]
	}
	if action == "" || action == "/" {
		action = "/list"
	}
	switch action {
	case "/list":
		a.handleFileList(w, r, server)
	case "/content":
		a.handleFileContent(w, r, server)
	case "/download":
		a.handleFileDownload(w, r, server)
	case "/upload":
		a.handleFileUpload(w, r, server)
	default:
		writeErr(w, r, http.StatusNotFound, "not_found", "File endpoint not found")
	}
}

func (a *App) handleFileList(w http.ResponseWriter, r *http.Request, server Server) {
	target, rel, err := a.safePath(server, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_path", err.Error())
		return
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	type item struct {
		Name    string    `json:"name"`
		Path    string    `json:"path"`
		Dir     bool      `json:"dir"`
		Size    int64     `json:"size"`
		ModTime time.Time `json:"modTime"`
	}
	out := []item{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, item{Name: entry.Name(), Path: filepath.ToSlash(filepath.Join(rel, entry.Name())), Dir: entry.IsDir(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": filepath.ToSlash(rel), "entries": out})
}

func (a *App) handleFileContent(w http.ResponseWriter, r *http.Request, server Server) {
	target, rel, err := a.safePath(server, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_path", err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		info, err := os.Stat(target)
		if err != nil || info.IsDir() {
			writeErr(w, r, http.StatusNotFound, "not_found", "File not found")
			return
		}
		if info.Size() > 2*1024*1024 {
			writeErr(w, r, http.StatusBadRequest, "file_too_large", "File is too large to edit")
			return
		}
		data, err := os.ReadFile(target)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "read_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": filepath.ToSlash(rel), "content": string(data)})
	case http.MethodPut:
		var req struct {
			Content string `json:"content"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if len(req.Content) > 2*1024*1024 {
			writeErr(w, r, http.StatusBadRequest, "file_too_large", "Content is too large")
			return
		}
		if err := os.WriteFile(target, []byte(req.Content), 0644); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "write_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true})
	case http.MethodDelete:
		if rel == "." {
			writeErr(w, r, http.StatusBadRequest, "bad_path", "Deleting the server root is not allowed")
			return
		}
		if err := os.RemoveAll(target); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "delete_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	default:
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
	}
}

func (a *App) handleFileDownload(w http.ResponseWriter, r *http.Request, server Server) {
	target, _, err := a.safePath(server, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_path", err.Error())
		return
	}
	http.ServeFile(w, r, target)
}

func (a *App) handleFileUpload(w http.ResponseWriter, r *http.Request, server Server) {
	if r.Method != http.MethodPost {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST")
		return
	}
	dir, _, err := a.safePath(server, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_path", err.Error())
		return
	}
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_upload", err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_upload", "Missing file")
		return
	}
	defer file.Close()
	name := filepath.Base(header.Filename)
	target, _, err := a.safePath(server, filepath.ToSlash(filepath.Join(r.URL.Query().Get("path"), name)))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_path", err.Error())
		return
	}
	if !strings.HasPrefix(target, dir) && target != filepath.Join(dir, name) {
		writeErr(w, r, http.StatusBadRequest, "bad_path", "Invalid target path")
		return
	}
	out, err := os.Create(target)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "write_failed", err.Error())
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "write_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"uploaded": name})
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request, server Server) {
	if r.Method != http.MethodGet {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET")
		return
	}
	dto := map[string]any{
		"running": a.processes.IsRunning(server.ID),
		"disk": map[string]any{
			"bytes": dirSize(a.absServerRoot(server)),
		},
	}
	if rt, ok := a.processes.Get(server.ID); ok && rt.PID() > 0 {
		p, err := process.NewProcess(int32(rt.PID()))
		if err == nil {
			mem, _ := p.MemoryInfo()
			cpu, _ := p.CPUPercent()
			ioCounters, _ := p.IOCounters()
			children, _ := p.Children()
			childPids := make([]int32, 0, len(children))
			for _, child := range children {
				childPids = append(childPids, child.Pid)
			}
			dto["process"] = map[string]any{
				"pid":      rt.PID(),
				"cpu":      cpu,
				"rss":      memoryRSS(mem),
				"io":       ioCounters,
				"children": childPids,
			}
		}
	}
	writeJSON(w, http.StatusOK, dto)
}

func (a *App) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET")
		return
	}
	status, err := a.latestRelease()
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, "update_check_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *App) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST")
		return
	}
	if a.processes.AnyRunning() {
		writeErr(w, r, http.StatusConflict, "servers_running", "Stop all servers before updating MyPanel")
		return
	}
	rel, err := a.latestRelease()
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, "update_check_failed", err.Error())
		return
	}
	assetURL, _ := rel["assetUrl"].(string)
	if assetURL == "" {
		writeErr(w, r, http.StatusNotFound, "asset_missing", "panel.exe asset was not found in latest release")
		return
	}
	target := filepath.Join(a.dataDir, "updates", "panel.exe.new")
	if err := a.download(assetURL, target); err != nil {
		writeErr(w, r, http.StatusBadGateway, "download_failed", err.Error())
		return
	}
	if runtime.GOOS != "windows" {
		writeJSON(w, http.StatusOK, map[string]any{"downloaded": target, "restart": "manual"})
		return
	}
	if err := a.spawnWindowsUpdater(target); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"updating": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
}

func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	sub, _ := fs.Sub(static.FS, "dist")
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if _, err := fs.Stat(sub, path); err != nil {
		path = "index.html"
	}
	data, err := fs.ReadFile(sub, path)
	if err != nil {
		writeErr(w, r, http.StatusNotFound, "not_found", "Static asset not found")
		return
	}
	if ctype := mime.TypeByExtension(filepath.Ext(path)); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	http.ServeContent(w, r, path, time.Now(), bytes.NewReader(data))
}

func (a *App) issueJWT(user User) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":  user.ID,
		"name": user.Username,
		"role": user.Role,
		"iat":  now.Unix(),
		"exp":  now.Add(8 * time.Hour).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(a.mustConfig().JWTSecret))
}

func (a *App) findUserByID(id string) (User, bool) {
	users, err := a.users.Get()
	if err != nil {
		return User{}, false
	}
	for _, user := range users {
		if user.ID == id {
			return user, true
		}
	}
	return User{}, false
}

func (a *App) findServer(id string) (Server, bool) {
	servers, err := a.servers.Get()
	if err != nil {
		return Server{}, false
	}
	for _, server := range servers {
		if server.ID == id || server.Slug == id {
			return server, true
		}
	}
	return Server{}, false
}

func (a *App) serverDTO(s Server) map[string]any {
	return map[string]any{
		"id":        s.ID,
		"name":      s.Name,
		"slug":      s.Slug,
		"root":      s.Root,
		"javaPath":  s.JavaPath,
		"jar":       s.Jar,
		"jvmArgs":   s.JVMArgs,
		"mcArgs":    s.MCArgs,
		"createdAt": s.CreatedAt,
		"updatedAt": s.UpdatedAt,
		"running":   a.processes.IsRunning(s.ID),
		"pid":       a.processes.PID(s.ID),
	}
}

func publicUser(user User) map[string]any {
	return map[string]any{"id": user.ID, "username": user.Username, "role": user.Role, "createdAt": user.CreatedAt}
}

func (a *App) absServerRoot(server Server) string {
	return filepath.Join(a.baseDir, filepath.FromSlash(server.Root))
}

func (a *App) safePath(server Server, raw string) (string, string, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" || raw == "." {
		raw = "."
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") || strings.Contains(raw, ":") || strings.HasPrefix(raw, "//") {
		return "", "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", "", errors.New("path traversal is not allowed")
	}
	root, err := filepath.Abs(a.absServerRoot(server))
	if err != nil {
		return "", "", err
	}
	resolvedRoot := root
	if rr, err := filepath.EvalSymlinks(root); err == nil {
		if resolvedRoot, err = filepath.Abs(rr); err != nil {
			return "", "", err
		}
	}
	target, err := filepath.Abs(filepath.Join(root, clean))
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", errors.New("path escapes server root")
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		resolvedAbs, _ := filepath.Abs(resolved)
		relResolved, err := filepath.Rel(resolvedRoot, resolvedAbs)
		if err != nil || relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(os.PathSeparator)) {
			return "", "", errors.New("symlink escapes server root")
		}
	}
	parent := filepath.Dir(target)
	for {
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			resolvedAbs, _ := filepath.Abs(resolved)
			relResolved, err := filepath.Rel(resolvedRoot, resolvedAbs)
			if err != nil || relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(os.PathSeparator)) {
				return "", "", errors.New("parent symlink escapes server root")
			}
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	return target, rel, nil
}

type ProcessManager struct {
	app      *App
	mu       sync.Mutex
	runtimes map[string]*Runtime
}

func NewProcessManager(app *App) *ProcessManager {
	return &ProcessManager{app: app, runtimes: map[string]*Runtime{}}
}

func (pm *ProcessManager) Start(server Server) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, ok := pm.runtimes[server.ID]; ok {
		return errors.New("server is already running")
	}
	root := pm.app.absServerRoot(server)
	jar, _, err := pm.app.safePath(server, server.Jar)
	if err != nil {
		return err
	}
	if _, err := os.Stat(jar); err != nil {
		return fmt.Errorf("jar not found: %s", server.Jar)
	}
	args := append([]string{}, server.JVMArgs...)
	args = append(args, "-jar", server.Jar)
	args = append(args, server.MCArgs...)
	cmd := exec.Command(server.JavaPath, args...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	rt := NewRuntime(cmd, stdin)
	if err := cmd.Start(); err != nil {
		return err
	}
	pm.runtimes[server.ID] = rt
	go rt.pipe(stdout)
	go rt.pipe(stderr)
	go func() {
		err := cmd.Wait()
		rt.Append(fmt.Sprintf("\r\n[process exited: %v]\r\n", err))
		pm.mu.Lock()
		delete(pm.runtimes, server.ID)
		pm.mu.Unlock()
		rt.Close()
	}()
	return nil
}

func (pm *ProcessManager) Stop(serverID string, timeout time.Duration) error {
	rt, ok := pm.Get(serverID)
	if !ok {
		return nil
	}
	_ = rt.Write([]byte("stop\n"))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pm.IsRunning(serverID) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return rt.Kill()
}

func (pm *ProcessManager) Get(serverID string) (*Runtime, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	rt, ok := pm.runtimes[serverID]
	return rt, ok
}

func (pm *ProcessManager) IsRunning(serverID string) bool {
	_, ok := pm.Get(serverID)
	return ok
}

func (pm *ProcessManager) AnyRunning() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return len(pm.runtimes) > 0
}

func (pm *ProcessManager) PID(serverID string) int {
	rt, ok := pm.Get(serverID)
	if !ok {
		return 0
	}
	return rt.PID()
}

type Runtime struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	subs    map[chan string]struct{}
	buffer  []string
	closed  bool
	maxBuff int
}

func NewRuntime(cmd *exec.Cmd, stdin io.WriteCloser) *Runtime {
	return &Runtime{cmd: cmd, stdin: stdin, subs: map[chan string]struct{}{}, maxBuff: 400}
}

func (rt *Runtime) PID() int {
	if rt.cmd.Process == nil {
		return 0
	}
	return rt.cmd.Process.Pid
}

func (rt *Runtime) Write(data []byte) error {
	rt.mu.Lock()
	stdin := rt.stdin
	rt.mu.Unlock()
	if stdin == nil {
		return errors.New("stdin closed")
	}
	_, err := stdin.Write(data)
	return err
}

func (rt *Runtime) Kill() error {
	if rt.cmd.Process == nil {
		return nil
	}
	return rt.cmd.Process.Kill()
}

func (rt *Runtime) pipe(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		rt.Append(scanner.Text() + "\r\n")
	}
}

func (rt *Runtime) Append(msg string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.buffer = append(rt.buffer, msg)
	if len(rt.buffer) > rt.maxBuff {
		rt.buffer = rt.buffer[len(rt.buffer)-rt.maxBuff:]
	}
	for ch := range rt.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (rt *Runtime) Buffer() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string{}, rt.buffer...)
}

func (rt *Runtime) Subscribe() chan string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ch := make(chan string, 64)
	rt.subs[ch] = struct{}{}
	return ch
}

func (rt *Runtime) Unsubscribe(ch chan string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.subs, ch)
	close(ch)
}

func (rt *Runtime) Close() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return
	}
	rt.closed = true
	for ch := range rt.subs {
		close(ch)
		delete(rt.subs, ch)
	}
	_ = rt.stdin.Close()
}

type JSONStore[T any] struct {
	path string
	mu   sync.Mutex
	def  T
}

func NewJSONStore[T any](path string, def T) *JSONStore[T] {
	return &JSONStore[T]{path: path, def: def}
}

func (s *JSONStore[T]) Get() (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *JSONStore[T]) Set(value T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(value)
}

func (s *JSONStore[T]) Update(fn func(T) (T, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := s.readLocked()
	if err != nil {
		return err
	}
	next, err := fn(value)
	if err != nil {
		return err
	}
	return s.writeLocked(next)
}

func (s *JSONStore[T]) readLocked() (T, error) {
	var value T
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if err := s.writeLocked(s.def); err != nil {
			return value, err
		}
		return s.def, nil
	}
	if err != nil {
		return value, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return s.def, nil
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, err
	}
	return value, nil
}

func (s *JSONStore[T]) writeLocked(value T) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (a *App) latestRelease() (map[string]any, error) {
	cfg := a.mustConfig()
	if cfg.GitHubRepo == "" {
		return map[string]any{"configured": false, "current": a.build.Version}, nil
	}
	url := "https://api.github.com/repos/" + cfg.GitHubRepo + "/releases/latest"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "MyPanel/"+a.build.Version)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var payload struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var assetURL string
	for _, asset := range payload.Assets {
		if strings.EqualFold(asset.Name, "panel.exe") {
			assetURL = asset.BrowserDownloadURL
			break
		}
	}
	return map[string]any{
		"configured": true,
		"current":    a.build.Version,
		"latest":     payload.TagName,
		"releaseUrl": payload.HTMLURL,
		"assetUrl":   assetURL,
		"available":  payload.TagName != "" && payload.TagName != a.build.Version,
	}, nil
}

func (a *App) download(url, target string) error {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "MyPanel/"+a.build.Version)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("download returned %s", resp.Status)
	}
	tmp := target + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	sum, _ := fileSHA256(tmp)
	log.Printf("downloaded update sha256=%s", sum)
	return os.Rename(tmp, target)
}

func (a *App) spawnWindowsUpdater(newExe string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	backup := filepath.Join(a.dataDir, "updates", "panel.exe.backup")
	script := filepath.Join(a.dataDir, "updates", "apply-update.ps1")
	content := fmt.Sprintf(`$ErrorActionPreference = "Stop"
Start-Sleep -Milliseconds 1200
Copy-Item -LiteralPath %q -Destination %q -Force
Move-Item -LiteralPath %q -Destination %q -Force
Start-Process -FilePath %q -WorkingDirectory %q
`, exe, backup, newExe, exe, exe, a.baseDir)
	if err := os.WriteFile(script, []byte(content), 0644); err != nil {
		return err
	}
	return exec.Command("powershell.exe", "-ExecutionPolicy", "Bypass", "-File", script).Start()
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeErr(w http.ResponseWriter, r *http.Request, code int, errCode, message string) {
	rid, _ := r.Context().Value(ctxRequestID{}).(string)
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": errCode, "message": message, "requestId": rid}})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(dst); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_json", "Invalid JSON body")
		return false
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

func newID(prefix string) string {
	return prefix + "_" + randomHex(8)
}

func cleanSlug(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	lastDash := false
	for _, r := range v {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func memoryRSS(info *process.MemoryInfoStat) uint64 {
	if info == nil {
		return 0
	}
	return info.RSS
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
