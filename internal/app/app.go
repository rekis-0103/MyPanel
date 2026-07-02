package app

import (
	"archive/zip"
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
	"net"
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

type AuditEntry struct {
	ID            string         `json:"id"`
	Time          time.Time      `json:"time"`
	ActorID       string         `json:"actorId"`
	ActorUsername string         `json:"actorUsername"`
	ActorRole     string         `json:"actorRole"`
	Action        string         `json:"action"`
	TargetType    string         `json:"targetType"`
	TargetID      string         `json:"targetId"`
	TargetName    string         `json:"targetName"`
	Method        string         `json:"method"`
	Path          string         `json:"path"`
	Status        int            `json:"status"`
	Detail        map[string]any `json:"detail,omitempty"`
}

type BackupEntry struct {
	ID        string    `json:"id"`
	ServerID  string    `json:"serverId"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"createdAt"`
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
	audit      *JSONStore[[]AuditEntry]
	backups    *JSONStore[[]BackupEntry]
	console    *ConsoleHub
	processes  *ProcessManager
	metrics    *MetricSampler
	updater    websocket.Upgrader
	http       *http.ServeMux
	client     *http.Client
}

const consoleMaxBytes = 5 * 1024 * 1024
const defaultGitHubRepo = "rekis-0103/MyPanel"
const hardcodedAdminID = "usr_hardcoded_admin"
const hardcodedAdminUsername = "admin"
const hardcodedAdminPasswordHash = "$2a$10$DXOuY0/yVzpEErUxhH7FDelxLDhvFiS5zmn8yTogvAy92ySDhnf2y"

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
	for _, dir := range []string{a.dataDir, a.serverRoot, filepath.Join(a.dataDir, "logs"), filepath.Join(a.dataDir, "updates"), filepath.Join(a.dataDir, "backups")} {
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
	a.audit = NewJSONStore(filepath.Join(a.dataDir, "audit.json"), []AuditEntry{})
	a.backups = NewJSONStore(filepath.Join(a.dataDir, "backups.json"), []BackupEntry{})
	if _, err := a.config.Get(); err != nil {
		return err
	}
	if _, err := a.users.Get(); err != nil {
		return err
	}
	if _, err := a.servers.Get(); err != nil {
		return err
	}
	if _, err := a.audit.Get(); err != nil {
		return err
	}
	if _, err := a.backups.Get(); err != nil {
		return err
	}
	a.console = NewConsoleHub(consoleMaxBytes)
	a.metrics = NewMetricSampler()
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

func (a *App) consoleHub() *ConsoleHub {
	if a.console == nil {
		a.console = NewConsoleHub(consoleMaxBytes)
	}
	return a.console
}

func (a *App) metricSampler() *MetricSampler {
	if a.metrics == nil {
		a.metrics = NewMetricSampler()
	}
	return a.metrics
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
	mux.HandleFunc("/api/v1/audit", a.auth(a.handleAudit, "admin"))
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

var _ http.Hijacker = (*statusWriter)(nil)

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
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
		bearer := strings.TrimPrefix(auth, "Bearer ")
		claims := jwt.MapClaims{}
		token, err := jwt.ParseWithClaims(bearer, claims, func(token *jwt.Token) (any, error) {
			if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, errors.New("unexpected signing method")
			}
			return []byte(a.mustConfig().JWTSecret), nil
		})
		if err == nil && token.Valid {
			userID, _ := claims["sub"].(string)
			user, ok := a.findUserByID(userID)
			if !ok || !roleAllows(user.Role, minRole) {
				writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, user)))
			return
		}
		writeErr(w, r, http.StatusUnauthorized, "unauthorized", "Invalid token")
	}
}

func roleAllows(role, min string) bool {
	role = normalizeRole(role)
	if min == "admin" {
		return role == "admin"
	}
	return role == "admin" || role == "user"
}

func currentUser(r *http.Request) User {
	user, _ := r.Context().Value(ctxUser{}).(User)
	return user
}

func normalizeRole(role string) string {
	if role == "admin" {
		return "admin"
	}
	return "user"
}

func hardcodedAdmin() User {
	return User{ID: hardcodedAdminID, Username: hardcodedAdminUsername, Role: "admin", CreatedAt: time.Unix(0, 0).UTC()}
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
		writeErr(w, r, http.StatusConflict, "setup_done", "Initial user already exists")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Username) < 3 || len(req.Password) < 8 || strings.EqualFold(req.Username, hardcodedAdminUsername) {
		writeErr(w, r, http.StatusBadRequest, "invalid_setup", "Username must be at least 3 characters and password at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "hash_error", err.Error())
		return
	}
	user := User{ID: newID("usr"), Username: req.Username, PasswordHash: string(hash), Role: "user", CreatedAt: time.Now()}
	if err := a.users.Set([]User{user}); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	a.addAudit(r, user, "setup.user", "user", user.ID, user.Username, http.StatusCreated, nil)
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
	if strings.EqualFold(req.Username, hardcodedAdminUsername) && bcrypt.CompareHashAndPassword([]byte(hardcodedAdminPasswordHash), []byte(req.Password)) == nil {
		user := hardcodedAdmin()
		token, err := a.issueJWT(user)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "token_error", err.Error())
			return
		}
		a.addAudit(r, user, "auth.login", "user", user.ID, user.Username, http.StatusOK, map[string]any{"success": true})
		writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": publicUser(user)})
		return
	}
	users, err := a.users.Get()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	for _, user := range users {
		if strings.EqualFold(user.Username, req.Username) && bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) == nil {
			user.Role = "user"
			token, err := a.issueJWT(user)
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, "token_error", err.Error())
				return
			}
			a.addAudit(r, user, "auth.login", "user", user.ID, user.Username, http.StatusOK, map[string]any{"success": true})
			writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": publicUser(user)})
			return
		}
	}
	a.addAudit(r, User{Username: strings.TrimSpace(req.Username), Role: "user"}, "auth.login_failed", "user", "", strings.TrimSpace(req.Username), http.StatusUnauthorized, map[string]any{"success": false})
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
		out := make([]map[string]any, 0, len(users)+1)
		out = append(out, publicUser(hardcodedAdmin()))
		for _, user := range users {
			user.Role = "user"
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
		req.Role = "user"
		if len(req.Username) < 3 || len(req.Password) < 8 || strings.EqualFold(req.Username, hardcodedAdminUsername) {
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
		a.addAudit(r, currentUser(r), "user.create", "user", created.ID, created.Username, http.StatusCreated, nil)
		writeJSON(w, http.StatusCreated, publicUser(created))
	default:
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
	}
}

func (a *App) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET")
		return
	}
	limit := queryInt(r, "limit", 100)
	offset := queryInt(r, "offset", 0)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	actor := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("actor")))
	role := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("role")))
	action := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("action")))
	serverID := strings.TrimSpace(r.URL.Query().Get("serverId"))
	entries, err := a.audit.Get()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	filtered := make([]AuditEntry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if actor != "" && !strings.Contains(strings.ToLower(entry.ActorUsername), actor) {
			continue
		}
		if role != "" && strings.ToLower(entry.ActorRole) != role {
			continue
		}
		if action != "" && !strings.Contains(strings.ToLower(entry.Action), action) {
			continue
		}
		if serverID != "" && entry.TargetID != serverID {
			continue
		}
		filtered = append(filtered, entry)
	}
	end := offset + limit
	if offset > len(filtered) {
		offset = len(filtered)
	}
	if end > len(filtered) {
		end = len(filtered)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": filtered[offset:end], "total": len(filtered), "limit": limit, "offset": offset})
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
		a.addAudit(r, currentUser(r), "config.update", "panel", "config", "Config", http.StatusOK, nil)
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
		a.addAudit(r, currentUser(r), "server.create", "server", created.ID, created.Name, http.StatusCreated, map[string]any{"slug": created.Slug})
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
	case "backups":
		a.handleBackups(w, r, server, parts[2:])
	case "logs":
		a.handleLogs(w, r, server)
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
		startupChange := req.JavaPath != nil || req.Jar != nil || req.JVMArgs != nil || req.MCArgs != nil
		if startupChange && a.processes.IsActive(server.ID) {
			writeErr(w, r, http.StatusConflict, "server_running", "Stop the server before changing startup settings")
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
		action := "server.update"
		if startupChange {
			action = "server.startup_update"
		}
		a.addAudit(r, currentUser(r), action, "server", updated.ID, updated.Name, http.StatusOK, nil)
		writeJSON(w, http.StatusOK, a.serverDTO(updated))
	case http.MethodDelete:
		if !roleAllows(currentUser(r).Role, "admin") {
			writeErr(w, r, http.StatusForbidden, "forbidden", "Permission denied")
			return
		}
		if a.processes.IsActive(server.ID) {
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
		a.addAudit(r, currentUser(r), "server.delete", "server", server.ID, server.Name, http.StatusOK, map[string]any{"deleteFiles": deleteFiles})
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
		err = a.processes.Stop(server.ID, 75*time.Second)
	default:
		writeErr(w, r, http.StatusBadRequest, "invalid_state", "State must be running or stopped")
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusConflict, "runtime_error", err.Error())
		return
	}
	action := "server.stop"
	if req.State == "running" {
		action = "server.start"
	}
	a.addAudit(r, currentUser(r), action, "server", server.ID, server.Name, http.StatusOK, nil)
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
	hub := a.consoleHub()
	snapshot, ch := hub.SubscribeWithSnapshot(server.ID)
	defer hub.Unsubscribe(server.ID, ch)
	for _, line := range snapshot {
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
			if rt, ok := a.processes.Get(server.ID); ok {
				_ = rt.Write(msg)
				a.addAudit(r, currentUser(r), "console.command", "server", server.ID, server.Name, http.StatusOK, map[string]any{"bytes": len(msg)})
			}
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
		a.addAudit(r, currentUser(r), "file.save", "file", server.ID, filepath.ToSlash(rel), http.StatusOK, nil)
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
		a.addAudit(r, currentUser(r), "file.delete", "file", server.ID, filepath.ToSlash(rel), http.StatusOK, nil)
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
	a.addAudit(r, currentUser(r), "file.upload", "file", server.ID, name, http.StatusCreated, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"uploaded": name})
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request, server Server) {
	if r.Method != http.MethodGet {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET")
		return
	}
	dto := map[string]any{
		"running":      a.processes.IsRunning(server.ID),
		"runtimeState": a.processes.State(server.ID),
		"sampledAt":    time.Now().UTC(),
		"disk": map[string]any{
			"bytes": dirSize(a.absServerRoot(server)),
		},
	}
	if rt, ok := a.processes.Get(server.ID); ok && rt.PID() > 0 {
		p, err := process.NewProcess(int32(rt.PID()))
		if err == nil {
			createTime, _ := p.CreateTime()
			usage := collectProcessTreeUsage(p)
			cpuValue := a.metricSampler().SampleValue(server.ID, p.Pid, createTime, time.Now(), usage.cpuSeconds)
			name, _ := p.Name()
			exe, _ := p.Exe()
			ioCounters, _ := p.IOCounters()
			dto["process"] = map[string]any{
				"pid":        rt.PID(),
				"name":       name,
				"exe":        exe,
				"createTime": createTime,
				"cpu":        cpuValue,
				"cpuMode":    "raw_multicore_tree",
				"rss":        usage.rss,
				"memoryMode": "rss_tree",
				"io":         ioCounters,
				"children":   usage.children,
			}
		}
	}
	writeJSON(w, http.StatusOK, dto)
}

func (a *App) handleBackups(w http.ResponseWriter, r *http.Request, server Server, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			entries, err := a.backups.Get()
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
				return
			}
			out := make([]BackupEntry, 0)
			for _, entry := range entries {
				if entry.ServerID == server.ID {
					out = append(out, entry)
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
			writeJSON(w, http.StatusOK, map[string]any{"backups": out})
		case http.MethodPost:
			if a.processes.IsActive(server.ID) {
				writeErr(w, r, http.StatusConflict, "server_running", "Stop the server before creating a backup")
				return
			}
			entry, err := a.createBackup(server)
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, "backup_failed", err.Error())
				return
			}
			a.addAudit(r, currentUser(r), "backup.create", "backup", entry.ID, entry.Name, http.StatusCreated, map[string]any{"serverId": server.ID, "size": entry.Size})
			writeJSON(w, http.StatusCreated, entry)
		default:
			writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
		}
		return
	}
	backupID := parts[0]
	entry, ok := a.findBackup(server.ID, backupID)
	if !ok {
		writeErr(w, r, http.StatusNotFound, "not_found", "Backup not found")
		return
	}
	if len(parts) > 1 && parts[1] == "download" {
		if r.Method != http.MethodGet {
			writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET")
			return
		}
		http.ServeFile(w, r, entry.Path)
		return
	}
	if len(parts) > 1 && parts[1] == "restore" {
		if r.Method != http.MethodPost {
			writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST")
			return
		}
		if a.processes.IsActive(server.ID) {
			writeErr(w, r, http.StatusConflict, "server_running", "Stop the server before restoring a backup")
			return
		}
		if err := a.restoreBackup(server, entry); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "restore_failed", err.Error())
			return
		}
		a.addAudit(r, currentUser(r), "backup.restore", "backup", entry.ID, entry.Name, http.StatusOK, map[string]any{"serverId": server.ID})
		writeJSON(w, http.StatusOK, map[string]any{"restored": true})
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
		return
	}
	if err := os.Remove(entry.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeErr(w, r, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	if err := a.backups.Update(func(entries []BackupEntry) ([]BackupEntry, error) {
		next := entries[:0]
		for _, item := range entries {
			if item.ID != entry.ID {
				next = append(next, item)
			}
		}
		return next, nil
	}); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	a.addAudit(r, currentUser(r), "backup.delete", "backup", entry.ID, entry.Name, http.StatusOK, map[string]any{"serverId": server.ID})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request, server Server) {
	if r.Method != http.MethodGet {
		writeErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET")
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		files := a.logFiles(server)
		writeJSON(w, http.StatusOK, map[string]any{"files": files})
		return
	}
	clean := filepath.ToSlash(filepath.Clean(strings.ReplaceAll(path, "\\", "/")))
	if clean != "logs/latest.log" && !strings.HasPrefix(clean, "crash-reports/") {
		writeErr(w, r, http.StatusBadRequest, "bad_path", "Only logs/latest.log and crash-reports are readable here")
		return
	}
	target, rel, err := a.safePath(server, clean)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_path", err.Error())
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		writeErr(w, r, http.StatusNotFound, "not_found", "Log file not found")
		return
	}
	if info.Size() > 4*1024*1024 {
		writeErr(w, r, http.StatusBadRequest, "file_too_large", "Log file is too large to view")
		return
	}
	data, err := os.ReadFile(target)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": filepath.ToSlash(rel), "content": string(data), "modTime": info.ModTime(), "size": info.Size()})
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
	if available, _ := rel["available"].(bool); !available {
		a.addAudit(r, currentUser(r), "update.apply_skipped", "panel", "mypanel", "MyPanel", http.StatusOK, map[string]any{"reason": "already_current"})
		writeJSON(w, http.StatusOK, map[string]any{
			"updating": false,
			"current":  rel["current"],
			"latest":   rel["latest"],
			"message":  "MyPanel is already up to date",
		})
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
		a.addAudit(r, currentUser(r), "update.download", "panel", "mypanel", "MyPanel", http.StatusOK, map[string]any{"target": target})
		writeJSON(w, http.StatusOK, map[string]any{"downloaded": target, "restart": "manual"})
		return
	}
	if err := a.spawnWindowsUpdater(target); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	a.addAudit(r, currentUser(r), "update.apply", "panel", "mypanel", "MyPanel", http.StatusAccepted, nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"updating": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
}

func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeErr(w, r, http.StatusNotFound, "not_found", "API route not found")
		return
	}
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
	w.Header().Set("Cache-Control", "no-store")
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
	if id == hardcodedAdminID {
		return hardcodedAdmin(), true
	}
	users, err := a.users.Get()
	if err != nil {
		return User{}, false
	}
	for _, user := range users {
		if user.ID == id {
			user.Role = "user"
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
		"id":           s.ID,
		"name":         s.Name,
		"slug":         s.Slug,
		"root":         s.Root,
		"javaPath":     s.JavaPath,
		"jar":          s.Jar,
		"jvmArgs":      s.JVMArgs,
		"mcArgs":       s.MCArgs,
		"createdAt":    s.CreatedAt,
		"updatedAt":    s.UpdatedAt,
		"running":      a.processes.IsRunning(s.ID),
		"runtimeState": a.processes.State(s.ID),
		"pid":          a.processes.PID(s.ID),
	}
}

func publicUser(user User) map[string]any {
	user.Role = normalizeRole(user.Role)
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
	if target == root {
		return target, rel, nil
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
		if parent == root {
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

func (a *App) addAudit(r *http.Request, actor User, action, targetType, targetID, targetName string, status int, detail map[string]any) {
	if a.audit == nil {
		return
	}
	if actor.ID == "" {
		actor.ID = "anonymous"
	}
	if actor.Role == "" {
		actor.Role = "user"
	}
	entry := AuditEntry{
		ID:            newID("aud"),
		Time:          time.Now().UTC(),
		ActorID:       actor.ID,
		ActorUsername: actor.Username,
		ActorRole:     normalizeRole(actor.Role),
		Action:        action,
		TargetType:    targetType,
		TargetID:      targetID,
		TargetName:    targetName,
		Status:        status,
		Detail:        detail,
	}
	if r != nil {
		entry.Method = r.Method
		entry.Path = r.URL.Path
	}
	if err := a.audit.Update(func(entries []AuditEntry) ([]AuditEntry, error) {
		entries = append(entries, entry)
		if len(entries) > 2000 {
			entries = entries[len(entries)-2000:]
		}
		return entries, nil
	}); err != nil {
		log.Printf("audit append failed: %v", err)
	}
}

func queryInt(r *http.Request, key string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return fallback
	}
	return value
}

func (a *App) createBackup(server Server) (BackupEntry, error) {
	id := newID("bak")
	name := fmt.Sprintf("%s-%s.zip", server.Slug, time.Now().Format("20060102-150405"))
	dir := filepath.Join(a.dataDir, "backups", server.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return BackupEntry{}, err
	}
	target := filepath.Join(dir, id+".zip")
	if err := zipDir(a.absServerRoot(server), target); err != nil {
		return BackupEntry{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return BackupEntry{}, err
	}
	entry := BackupEntry{ID: id, ServerID: server.ID, Name: name, Path: target, Size: info.Size(), CreatedAt: time.Now().UTC()}
	if err := a.backups.Update(func(entries []BackupEntry) ([]BackupEntry, error) {
		return append(entries, entry), nil
	}); err != nil {
		return BackupEntry{}, err
	}
	return entry, nil
}

func (a *App) findBackup(serverID, backupID string) (BackupEntry, bool) {
	entries, err := a.backups.Get()
	if err != nil {
		return BackupEntry{}, false
	}
	for _, entry := range entries {
		if entry.ServerID == serverID && entry.ID == backupID {
			return entry, true
		}
	}
	return BackupEntry{}, false
}

func (a *App) restoreBackup(server Server, entry BackupEntry) error {
	root := a.absServerRoot(server)
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	return unzipTo(entry.Path, root)
}

func (a *App) logFiles(server Server) []map[string]any {
	paths := []string{"logs/latest.log"}
	crashDir := filepath.Join(a.absServerRoot(server), "crash-reports")
	if entries, err := os.ReadDir(crashDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".txt") {
				paths = append(paths, filepath.ToSlash(filepath.Join("crash-reports", entry.Name())))
			}
		}
	}
	out := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		target, rel, err := a.safePath(server, path)
		if err != nil {
			continue
		}
		info, err := os.Stat(target)
		if err != nil || info.IsDir() {
			continue
		}
		out = append(out, map[string]any{"path": filepath.ToSlash(rel), "name": filepath.Base(rel), "size": info.Size(), "modTime": info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		ti, _ := out[i]["modTime"].(time.Time)
		tj, _ := out[j]["modTime"].(time.Time)
		return ti.After(tj)
	})
	return out
}

func zipDir(root, target string) error {
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = rel
		if entry.IsDir() {
			header.Name += "/"
			_, err = zw.CreateHeader(header)
			return err
		}
		header.Method = zip.Deflate
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	})
}

func unzipTo(source, root string) error {
	reader, err := zip.OpenReader(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	for _, file := range reader.File {
		name := filepath.Clean(filepath.FromSlash(file.Name))
		if name == "." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe zip entry: %s", file.Name)
		}
		target := filepath.Join(rootAbs, name)
		targetAbs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(rootAbs, targetAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry escapes server root: %s", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(targetAbs, file.Mode()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetAbs), 0755); err != nil {
			return err
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(targetAbs, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, file.Mode())
		if err != nil {
			src.Close()
			return err
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

type ConsoleHub struct {
	mu      sync.Mutex
	maxByte int
	logs    map[string]*ConsoleLog
}

type ConsoleLog struct {
	messages []string
	bytes    int
	subs     map[chan string]struct{}
}

func NewConsoleHub(maxByte int) *ConsoleHub {
	return &ConsoleHub{maxByte: maxByte, logs: map[string]*ConsoleLog{}}
}

func (hub *ConsoleHub) Append(serverID, msg string) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	log := hub.logLocked(serverID)
	if hub.maxByte > 0 && len(msg) > hub.maxByte {
		msg = msg[len(msg)-hub.maxByte:]
	}
	log.messages = append(log.messages, msg)
	log.bytes += len(msg)
	for hub.maxByte > 0 && log.bytes > hub.maxByte && len(log.messages) > 0 {
		removed := log.messages[0]
		log.messages = log.messages[1:]
		log.bytes -= len(removed)
	}
	for ch := range log.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (hub *ConsoleHub) Snapshot(serverID string) []string {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	log := hub.logLocked(serverID)
	return append([]string{}, log.messages...)
}

func (hub *ConsoleHub) Subscribe(serverID string) chan string {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	ch := make(chan string, 64)
	hub.logLocked(serverID).subs[ch] = struct{}{}
	return ch
}

func (hub *ConsoleHub) SubscribeWithSnapshot(serverID string) ([]string, chan string) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	log := hub.logLocked(serverID)
	ch := make(chan string, 64)
	log.subs[ch] = struct{}{}
	return append([]string{}, log.messages...), ch
}

func (hub *ConsoleHub) Unsubscribe(serverID string, ch chan string) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	log := hub.logLocked(serverID)
	if _, ok := log.subs[ch]; ok {
		delete(log.subs, ch)
		close(ch)
	}
}

func (hub *ConsoleHub) logLocked(serverID string) *ConsoleLog {
	log, ok := hub.logs[serverID]
	if !ok {
		log = &ConsoleLog{subs: map[chan string]struct{}{}}
		hub.logs[serverID] = log
	}
	return log
}

func serverCommandArgs(server Server) []string {
	args := append([]string{}, server.JVMArgs...)
	args = append(args, "-jar", server.Jar)
	return append(args, server.MCArgs...)
}

func commandDisplay(binary string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteCommandPart(binary))
	for _, arg := range args {
		parts = append(parts, quoteCommandPart(arg))
	}
	return strings.Join(parts, " ")
}

func quoteCommandPart(part string) string {
	if part == "" || strings.ContainsAny(part, " \t\r\n\"") {
		return strconv.Quote(part)
	}
	return part
}

func detectMinecraftReady(line string) bool {
	return strings.Contains(line, "Done (") && strings.Contains(line, `For help, type "help"`)
}

func detectMinecraftStopping(line string) bool {
	return strings.Contains(line, "All RegionFile I/O tasks to complete") ||
		strings.Contains(line, "[MoonriseCommon] Awaiting termination of worker pool") ||
		strings.Contains(line, "[MoonriseCommon] Awaiting termination of I/O pool")
}

type ProcessManager struct {
	app      *App
	mu       sync.Mutex
	runtimes map[string]*Runtime
}

const (
	runtimeStopped  = "stopped"
	runtimeStarting = "starting"
	runtimeRunning  = "running"
	runtimeStopping = "stopping"
)

func NewProcessManager(app *App) *ProcessManager {
	return &ProcessManager{app: app, runtimes: map[string]*Runtime{}}
}

func (pm *ProcessManager) Start(server Server) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, ok := pm.runtimes[server.ID]; ok {
		return errors.New("server is already running")
	}
	hub := pm.app.consoleHub()
	args := serverCommandArgs(server)
	hub.Append(server.ID, fmt.Sprintf("\r\n[MyPanel] starting: %s\r\n", commandDisplay(server.JavaPath, args)))
	root := pm.app.absServerRoot(server)
	jar, _, err := pm.app.safePath(server, server.Jar)
	if err != nil {
		hub.Append(server.ID, fmt.Sprintf("[MyPanel] start failed: %v\r\n", err))
		return err
	}
	if _, err := os.Stat(jar); err != nil {
		err = fmt.Errorf("jar not found: %s", server.Jar)
		hub.Append(server.ID, fmt.Sprintf("[MyPanel] start failed: %v\r\n", err))
		return err
	}
	cmd := exec.Command(server.JavaPath, args...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		hub.Append(server.ID, fmt.Sprintf("[MyPanel] start failed: %v\r\n", err))
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		hub.Append(server.ID, fmt.Sprintf("[MyPanel] start failed: %v\r\n", err))
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		hub.Append(server.ID, fmt.Sprintf("[MyPanel] start failed: %v\r\n", err))
		return err
	}
	rt := NewRuntime(cmd, stdin)
	if err := cmd.Start(); err != nil {
		hub.Append(server.ID, fmt.Sprintf("[MyPanel] start failed: %v\r\n", err))
		return err
	}
	rt.SetState(runtimeStarting)
	hub.Append(server.ID, fmt.Sprintf("[MyPanel] server process started pid=%d\r\n", cmd.Process.Pid))
	pm.runtimes[server.ID] = rt
	appendLine := func(msg string) {
		hub.Append(server.ID, msg)
		if detectMinecraftReady(msg) && rt.MarkRunning() {
			hub.Append(server.ID, "[MyPanel] server marked as running\r\n")
		}
		if detectMinecraftStopping(msg) {
			rt.MarkStopping()
		}
	}
	go rt.pipe(stdout, appendLine)
	go rt.pipe(stderr, appendLine)
	go func() {
		err := cmd.Wait()
		if err != nil {
			hub.Append(server.ID, fmt.Sprintf("\r\n[MyPanel] process exited: %v\r\n", err))
		} else {
			hub.Append(server.ID, "\r\n[MyPanel] process exited: code=0\r\n")
		}
		rt.SetState(runtimeStopped)
		hub.Append(server.ID, "[MyPanel] server marked as stopped\r\n")
		pm.mu.Lock()
		delete(pm.runtimes, server.ID)
		pm.mu.Unlock()
		rt.Close()
	}()
	return nil
}

func (pm *ProcessManager) Stop(serverID string, timeout time.Duration) error {
	rt, ok := pm.Get(serverID)
	hub := pm.app.consoleHub()
	if !ok {
		hub.Append(serverID, "[MyPanel] stop requested but server is not running\r\n")
		return nil
	}
	rt.MarkStopping()
	hub.Append(serverID, "[MyPanel] stopping: sending \"stop\"\r\n")
	if err := rt.Write([]byte("stop\n")); err != nil {
		hub.Append(serverID, fmt.Sprintf("[MyPanel] stop write failed: %v\r\n", err))
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pm.IsActive(serverID) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	hub.Append(serverID, "[MyPanel] stop timeout; killing process\r\n")
	if err := rt.Kill(); err != nil {
		hub.Append(serverID, fmt.Sprintf("[MyPanel] kill failed: %v\r\n", err))
		return err
	}
	return nil
}

func (pm *ProcessManager) Get(serverID string) (*Runtime, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	rt, ok := pm.runtimes[serverID]
	return rt, ok
}

func (pm *ProcessManager) IsRunning(serverID string) bool {
	return pm.State(serverID) == runtimeRunning
}

func (pm *ProcessManager) IsActive(serverID string) bool {
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

func (pm *ProcessManager) State(serverID string) string {
	rt, ok := pm.Get(serverID)
	if !ok {
		return runtimeStopped
	}
	return rt.State()
}

type Runtime struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
	closed bool
	state  string
}

func NewRuntime(cmd *exec.Cmd, stdin io.WriteCloser) *Runtime {
	return &Runtime{cmd: cmd, stdin: stdin, state: runtimeStarting}
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

func (rt *Runtime) State() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.state == "" {
		return runtimeStopped
	}
	return rt.state
}

func (rt *Runtime) SetState(state string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.state = state
}

func (rt *Runtime) MarkRunning() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.state == runtimeRunning || rt.state == runtimeStopping || rt.state == runtimeStopped {
		return false
	}
	rt.state = runtimeRunning
	return true
}

func (rt *Runtime) MarkStopping() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.state != runtimeStopped {
		rt.state = runtimeStopping
	}
}

func (rt *Runtime) Kill() error {
	if rt.cmd.Process == nil {
		return nil
	}
	return rt.cmd.Process.Kill()
}

func (rt *Runtime) pipe(reader io.Reader, appendMsg func(string)) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		appendMsg(scanner.Text() + "\r\n")
	}
}

func (rt *Runtime) Close() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return
	}
	rt.closed = true
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
	repo := defaultGitHubRepo
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
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
		"repo":       repo,
		"current":    a.build.Version,
		"latest":     payload.TagName,
		"releaseUrl": payload.HTMLURL,
		"assetUrl":   assetURL,
		"available":  payload.TagName != "" && !sameVersion(payload.TagName, a.build.Version),
	}, nil
}

func sameVersion(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") == strings.TrimPrefix(strings.TrimSpace(b), "v")
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

type processTreeUsage struct {
	rss        uint64
	cpuSeconds float64
	children   []int32
}

func collectProcessTreeUsage(root *process.Process) processTreeUsage {
	var usage processTreeUsage
	seen := map[int32]struct{}{}
	var walk func(proc *process.Process, rootPID int32)
	walk = func(proc *process.Process, rootPID int32) {
		if proc == nil {
			return
		}
		if _, ok := seen[proc.Pid]; ok {
			return
		}
		seen[proc.Pid] = struct{}{}
		if proc.Pid != rootPID {
			usage.children = append(usage.children, proc.Pid)
		}
		if mem, err := proc.MemoryInfo(); err == nil {
			usage.rss += memoryRSS(mem)
		}
		if times, err := proc.Times(); err == nil {
			usage.cpuSeconds += times.User + times.System
		}
		children, err := proc.Children()
		if err != nil {
			return
		}
		for _, child := range children {
			walk(child, rootPID)
		}
	}
	walk(root, root.Pid)
	sort.Slice(usage.children, func(i, j int) bool { return usage.children[i] < usage.children[j] })
	return usage
}

type MetricSampler struct {
	mu      sync.Mutex
	samples map[string]metricSample
}

type metricSample struct {
	pid        int32
	createTime int64
	wall       time.Time
	cpuSeconds float64
}

func NewMetricSampler() *MetricSampler {
	return &MetricSampler{samples: map[string]metricSample{}}
}

func (sampler *MetricSampler) Sample(serverID string, proc *process.Process, createTime int64, now time.Time) (float64, error) {
	times, err := proc.Times()
	if err != nil {
		return 0, err
	}
	return sampler.SampleValue(serverID, proc.Pid, createTime, now, times.User+times.System), nil
}

func (sampler *MetricSampler) SampleValue(serverID string, pid int32, createTime int64, now time.Time, cpuSeconds float64) float64 {
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	next := metricSample{pid: pid, createTime: createTime, wall: now, cpuSeconds: cpuSeconds}
	prev, ok := sampler.samples[serverID]
	sampler.samples[serverID] = next
	if !ok || prev.pid != pid || prev.createTime != createTime {
		return 0
	}
	wallSeconds := now.Sub(prev.wall).Seconds()
	cpuDelta := cpuSeconds - prev.cpuSeconds
	if wallSeconds <= 0 || cpuDelta < 0 {
		return 0
	}
	return (cpuDelta / wallSeconds) * 100
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
