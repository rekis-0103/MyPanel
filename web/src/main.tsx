import React, { useEffect, useRef, useState } from "react";
import ReactDOM from "react-dom/client";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import {
  Activity,
  Download,
  ExternalLink,
  FileText,
  Folder,
  HardDrive,
  LogOut,
  Moon,
  Plus,
  Power,
  RefreshCcw,
  Save,
  Send,
  Server as ServerIcon,
  Shield,
  Square,
  Sun,
  TerminalSquare,
  Trash2,
  Upload
} from "lucide-react";
import "./styles.css";

type Role = "viewer" | "operator" | "admin";

type User = {
  id: string;
  username: string;
  role: Role;
};

type Server = {
  id: string;
  name: string;
  slug: string;
  root: string;
  javaPath: string;
  jar: string;
  jvmArgs: string[];
  mcArgs: string[];
  running: boolean;
  pid: number;
};

type FileEntry = {
  name: string;
  path: string;
  dir: boolean;
  size: number;
  modTime: string;
};

type Metrics = {
  running: boolean;
  sampledAt: string;
  disk: { bytes: number };
  process?: {
    pid: number;
    name: string;
    exe: string;
    createTime: number;
    cpu: number;
    cpuMode?: string;
    rss: number;
    memoryMode?: string;
    children: number[];
  };
};

type MetricSample = {
  ts: number;
  cpu: number;
  rss: number;
  disk: number;
  running: boolean;
};

type Lang = "en" | "id";
type Theme = "light" | "dark";
type Copy = typeof copy.en;

const apiBase = "/api/v1";

function getInitialTheme(): Theme {
  return localStorage.getItem("mypanel.theme") === "dark" ? "dark" : "light";
}

const copy = {
  en: {
    appSubtitle: "Server operations console",
    controlPlane: "Control plane",
    overviewCopy: "Manage local Minecraft instances, inspect files, and keep process telemetry in one portable panel.",
    noServerTitle: "Register a Minecraft server",
    registerServer: "Register server",
    instances: "Instances",
    managedInstances: "Managed instances",
    localRuntime: "Local runtime",
    portableBinary: "Portable binary",
    jwtSecured: "JWT secured",
    tunnelReady: "Tunnel ready",
    noServers: "Register a folder from server/ to start managing an instance.",
    refresh: "Refresh",
    console: "Console",
    files: "Files",
    metrics: "Metrics",
    update: "Update",
    status: "Status",
    processId: "Process ID",
    jarFile: "Jar file",
    workingFolder: "Working folder",
    loginTitle: "Sign in to MyPanel",
    setupTitle: "Create the first admin",
    username: "Username",
    password: "Password",
    signIn: "Sign in",
    createAdmin: "Create admin",
    setupFirst: "First run setup",
    backToLogin: "Back to login",
    addAnother: "Add another server",
    formTitle: "Register server",
    formHelp: "Use an existing folder name inside the server/ directory.",
    serverName: "Server name",
    folder: "Folder",
    ram: "RAM",
    create: "Create",
    upload: "Upload",
    emptyFolder: "This folder is empty.",
    chooseText: "Choose a text file",
    save: "Save",
    stopped: "Stopped",
    running: "Running",
    ramRss: "RAM RSS",
    folderSize: "Folder size",
    selfUpdate: "Self-update",
    openReleases: "Open releases",
    checkUpdate: "Check update",
    updateAvailable: "Update available",
    noUpdateAvailable: "Panel is already up to date.",
    updateConfirm: "A new MyPanel release is available. Download and install it now?",
    applyUpdate: "Download and install update",
    logout: "Logout",
    liveConsole: "Live console",
    websocketSession: "WebSocket stream",
    fileBrowser: "File browser",
    fileEditor: "Editor",
    nameColumn: "Name",
    sizeColumn: "Size",
    processMonitor: "Process monitor",
    currentSession: "Current session",
    connectionReady: "Ready",
    language: "Language",
    theme: "Theme",
    lightTheme: "Light mode",
    darkTheme: "Dark mode",
    authPortableLabel: "Runtime",
    authSecurityLabel: "Access",
    authTunnelLabel: "Tunnel",
    jvmProcess: "JVM process",
    samplingEverySecond: "1s sampling",
    last60Seconds: "Last 60 seconds",
    waitingForSamples: "Start the server to collect JVM samples.",
    consoleConnecting: "Connecting",
    consoleConnected: "Connected",
    consoleHistory: "History",
    consoleLive: "Live",
    consoleDisconnected: "Disconnected",
    consoleStopped: "Server is not running. Press Start to open a live console.",
    consoleCommandPlaceholder: "Type a server command or message",
    sendCommand: "Send"
  },
  id: {
    appSubtitle: "Konsol operasi server",
    controlPlane: "Control plane",
    overviewCopy: "Kelola instance Minecraft lokal, inspeksi file, dan pantau proses dari satu panel portable.",
    noServerTitle: "Daftarkan server Minecraft",
    registerServer: "Daftarkan server",
    instances: "Instance",
    managedInstances: "Instance terkelola",
    localRuntime: "Runtime lokal",
    portableBinary: "Binary portable",
    jwtSecured: "JWT aman",
    tunnelReady: "Siap tunnel",
    noServers: "Daftarkan folder dari server/ untuk mulai mengelola instance.",
    refresh: "Segarkan",
    console: "Konsol",
    files: "Berkas",
    metrics: "Metrik",
    update: "Update",
    status: "Status",
    processId: "Process ID",
    jarFile: "File jar",
    workingFolder: "Folder kerja",
    loginTitle: "Masuk ke MyPanel",
    setupTitle: "Buat admin pertama",
    username: "Username",
    password: "Password",
    signIn: "Masuk",
    createAdmin: "Buat admin",
    setupFirst: "Setup pertama kali",
    backToLogin: "Kembali ke login",
    addAnother: "Tambah server lain",
    formTitle: "Daftarkan server",
    formHelp: "Gunakan nama folder yang sudah ada di direktori server/.",
    serverName: "Nama server",
    folder: "Folder",
    ram: "RAM",
    create: "Buat",
    upload: "Upload",
    emptyFolder: "Folder ini kosong.",
    chooseText: "Pilih file teks",
    save: "Simpan",
    stopped: "Stopped",
    running: "Running",
    ramRss: "RAM RSS",
    folderSize: "Ukuran folder",
    selfUpdate: "Self-update",
    openReleases: "Buka releases",
    checkUpdate: "Cek update",
    updateAvailable: "Update tersedia",
    noUpdateAvailable: "Versi panel sudah terbaru.",
    updateConfirm: "Release MyPanel baru tersedia. Unduh dan pasang sekarang?",
    applyUpdate: "Unduh dan pasang update",
    logout: "Logout",
    liveConsole: "Konsol live",
    websocketSession: "Stream WebSocket",
    fileBrowser: "File browser",
    fileEditor: "Editor",
    nameColumn: "Nama",
    sizeColumn: "Ukuran",
    processMonitor: "Monitor proses",
    currentSession: "Sesi aktif",
    connectionReady: "Siap",
    language: "Bahasa",
    theme: "Tema",
    lightTheme: "Mode terang",
    darkTheme: "Mode gelap",
    authPortableLabel: "Runtime",
    authSecurityLabel: "Akses",
    authTunnelLabel: "Tunnel",
    jvmProcess: "Proses JVM",
    samplingEverySecond: "Sampling 1 detik",
    last60Seconds: "60 detik terakhir",
    waitingForSamples: "Jalankan server untuk mengumpulkan sampel JVM.",
    consoleConnecting: "Menghubungkan",
    consoleConnected: "Terhubung",
    consoleHistory: "Riwayat",
    consoleLive: "Live",
    consoleDisconnected: "Terputus",
    consoleStopped: "Server belum berjalan. Tekan Start untuk membuka konsol live.",
    consoleCommandPlaceholder: "Ketik command atau pesan server",
    sendCommand: "Kirim"
  }
};

function App() {
  const [token, setToken] = useState(() => localStorage.getItem("mypanel.token") || "");
  const [user, setUser] = useState<User | null>(null);
  const [servers, setServers] = useState<Server[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [view, setView] = useState<"console" | "files" | "metrics">("console");
  const [error, setError] = useState("");
  const [lang, setLang] = useState<Lang>(() => (localStorage.getItem("mypanel.lang") as Lang) || "en");
  const [theme, setTheme] = useState<Theme>(getInitialTheme);
  const t = copy[lang];

  const selected = servers.find((server) => server.id === selectedId) || servers[0];

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
  }, [theme]);

  function toggleLang() {
    const next = lang === "en" ? "id" : "en";
    localStorage.setItem("mypanel.lang", next);
    setLang(next);
  }

  function toggleTheme() {
    const next = theme === "light" ? "dark" : "light";
    localStorage.setItem("mypanel.theme", next);
    setTheme(next);
  }

  async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = new Headers(init.headers);
    if (!headers.has("Content-Type") && !(init.body instanceof FormData)) {
      headers.set("Content-Type", "application/json");
    }
    if (token) headers.set("Authorization", `Bearer ${token}`);
    const res = await fetch(`${apiBase}${path}`, { ...init, headers });
    if (!res.ok) {
      let message = `Request failed: ${res.status}`;
      try {
        const body = await res.json();
        message = body.error?.message || message;
      } catch {
        // keep default
      }
      throw new Error(message);
    }
    return res.json();
  }

  async function refresh() {
    if (!token) return;
    const [me, list] = await Promise.all([api<User>("/me"), api<Server[]>("/servers")]);
    setUser(me);
    setServers(list);
    if (!selectedId && list.length) setSelectedId(list[0].id);
  }

  useEffect(() => {
    refresh().catch(() => {
      localStorage.removeItem("mypanel.token");
      setToken("");
      setUser(null);
    });
  }, [token]);

  function loginDone(nextToken: string) {
    localStorage.setItem("mypanel.token", nextToken);
    setToken(nextToken);
  }

  function logout() {
    localStorage.removeItem("mypanel.token");
    setToken("");
    setUser(null);
    setServers([]);
  }

  if (!token || !user) {
    return <AuthScreen onToken={loginDone} lang={lang} theme={theme} t={t} onToggleLang={toggleLang} onToggleTheme={toggleTheme} />;
  }

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="sidebar-top">
          <div className="brand">
            <div className="cube-mark">MP</div>
            <div>
              <strong>MyPanel</strong>
              <span>{t.appSubtitle}</span>
            </div>
          </div>
          <span className="build-badge">LOCAL</span>
        </div>

        <button className="new-server" onClick={() => setView("console")}>
          <Plus size={16} /> {t.registerServer}
        </button>

        {user.role === "admin" && <UpdateWidget api={api} t={t} />}

        <div className="sidebar-stat">
          <span>{t.managedInstances}</span>
          <strong>{servers.length}</strong>
        </div>

        <div className="nav-label">{t.instances}</div>
        <div className="server-list">
          {servers.map((server) => (
            <button
              key={server.id}
              className={`server-row ${selected?.id === server.id ? "active" : ""}`}
              onClick={() => setSelectedId(server.id)}
            >
              <ServerIcon size={17} />
              <span className="server-row-text">
                <strong>{server.name}</strong>
                <small>server/{server.slug}</small>
              </span>
              <span className={`server-state ${server.running ? "running" : ""}`}>{server.running ? t.running : t.stopped}</span>
              <i className={server.running ? "dot on" : "dot"} />
            </button>
          ))}
          {servers.length === 0 && <p className="empty">{t.noServers}</p>}
        </div>

        {selected && (
          <nav className="sidebar-tabs" aria-label={t.currentSession}>
            <button className={view === "console" ? "active" : ""} onClick={() => setView("console")}>
              <TerminalSquare size={16} /> {t.console}
            </button>
            <button className={view === "files" ? "active" : ""} onClick={() => setView("files")}>
              <Folder size={16} /> {t.files}
            </button>
            <button className={view === "metrics" ? "active" : ""} onClick={() => setView("metrics")}>
              <Activity size={16} /> {t.metrics}
            </button>
          </nav>
        )}

        <div className="identity">
          <Shield size={16} />
          <span>{user.username}</span>
          <b>{user.role}</b>
          <button className="icon-button lang-button" onClick={toggleLang} title={t.language}>
            {lang.toUpperCase()}
          </button>
          <ThemeToggle theme={theme} label={`${t.theme}: ${theme === "light" ? t.darkTheme : t.lightTheme}`} onClick={toggleTheme} />
          <button className="icon-button" onClick={logout} title={t.logout}>
            <LogOut size={16} />
          </button>
        </div>
      </aside>

      <main className="workspace">
        <header className="topbar">
          <div>
            <span className="section-kicker">{t.controlPlane}</span>
            <span className="eyebrow">server/{selected?.slug || "new"}</span>
            <h1>{selected?.name || t.noServerTitle}</h1>
            <p>{t.overviewCopy}</p>
          </div>
          <div className="actions">
            <button onClick={() => refresh().catch((err) => setError(err.message))}>
              <RefreshCcw size={16} /> {t.refresh}
            </button>
            {selected && (
              <RuntimeButton server={selected} api={api} onDone={refresh} />
            )}
          </div>
        </header>

        {error && <div className="alert">{error}</div>}

        {!selected ? (
          <CreateServer api={api} onDone={refresh} t={t} />
        ) : (
          <>
            <StatusDeck selected={selected} servers={servers} t={t} />

            <section className="command-panel">
              <div className="command-panel-head">
                <div>
                  <span className="section-kicker">{t.currentSession}</span>
                  <strong>{selected.name}</strong>
                </div>
              </div>

              <div className="command-panel-body">
                {view === "console" && <Console server={selected} token={token} t={t} />}
                {view === "files" && <Files server={selected} api={api} token={token} t={t} />}
                {view === "metrics" && <MetricsView server={selected} api={api} t={t} />}
              </div>
            </section>

            <CreateServer compact api={api} onDone={refresh} t={t} />
          </>
        )}
      </main>
    </div>
  );
}

function StatusDeck({ selected, servers, t }: { selected: Server; servers: Server[]; t: Copy }) {
  const runningCount = servers.filter((server) => server.running).length;

  return (
    <section className="status-deck" aria-label="Server status summary">
      <div className={`status-hero ${selected.running ? "online" : ""}`}>
        <div>
          <span className="section-kicker">{t.status}</span>
          <strong>{selected.running ? t.running : t.stopped}</strong>
        </div>
        <div className="signal-stack" aria-hidden="true">
          <span />
          <span />
          <span />
        </div>
      </div>
      <InfoTile label={t.managedInstances} value={`${runningCount}/${servers.length}`} />
      <InfoTile label={t.processId} value={selected.pid ? String(selected.pid) : "-"} />
      <InfoTile label={t.jarFile} value={selected.jar} mono />
      <InfoTile label={t.workingFolder} value={selected.root} mono wide />
    </section>
  );
}

function AuthFeature({ label, value }: { label: string; value: string }) {
  return (
    <div className="auth-feature">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function LanguageToggle({ lang, label, onClick }: { lang: Lang; label: string; onClick: () => void }) {
  return (
    <button className="language-toggle" type="button" onClick={onClick} aria-label={label}>
      <span>{lang === "en" ? "EN" : "ID"}</span>
      <i>{lang === "en" ? "ID" : "EN"}</i>
    </button>
  );
}

function ThemeToggle({ theme, label, onClick }: { theme: Theme; label: string; onClick: () => void }) {
  return (
    <button className="icon-button theme-button" type="button" onClick={onClick} title={label} aria-label={label}>
      {theme === "light" ? <Moon size={16} /> : <Sun size={16} />}
    </button>
  );
}

function InfoTile({ label, value, tone = "default", mono = false, wide = false }: { label: string; value: string; tone?: "default" | "good" | "muted"; mono?: boolean; wide?: boolean }) {
  return (
    <div className={`info-tile ${tone} ${wide ? "wide" : ""}`}>
      <span>{label}</span>
      <strong className={mono ? "mono" : ""}>{value}</strong>
    </div>
  );
}

function AuthScreen({
  onToken,
  lang,
  theme,
  t,
  onToggleLang,
  onToggleTheme
}: {
  onToken: (token: string) => void;
  lang: Lang;
  theme: Theme;
  t: Copy;
  onToggleLang: () => void;
  onToggleTheme: () => void;
}) {
  const [mode, setMode] = useState<"login" | "setup">("login");
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setError("");
    try {
      if (mode === "setup") {
        await fetch(`${apiBase}/setup`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ username, password })
        }).then(async (res) => {
          if (!res.ok) throw new Error((await res.json()).error?.message || "Setup failed");
        });
      }
      const body = await fetch(`${apiBase}/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password })
      }).then(async (res) => {
        if (!res.ok) throw new Error((await res.json()).error?.message || "Login failed");
        return res.json();
      });
      onToken(body.token);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Request failed");
    }
  }

  return (
    <div className="auth-page">
      <div className="auth-shell">
        <aside className="auth-context">
          <div className="brand large">
            <div className="cube-mark">MP</div>
            <div>
              <strong>MyPanel</strong>
              <span>{t.appSubtitle}</span>
            </div>
          </div>
          <div className="auth-command-card">
            <span className="section-kicker">{t.localRuntime}</span>
            <h2>Portable Minecraft control surface</h2>
            <p>{t.overviewCopy}</p>
            <div className="auth-feature-grid">
              <AuthFeature label={t.authPortableLabel} value={t.portableBinary} />
              <AuthFeature label={t.authSecurityLabel} value={t.jwtSecured} />
              <AuthFeature label={t.authTunnelLabel} value={t.tunnelReady} />
            </div>
          </div>
        </aside>

        <form className="auth-panel" onSubmit={submit}>
          <div className="auth-controls">
            <ThemeToggle theme={theme} label={`${t.theme}: ${theme === "light" ? t.darkTheme : t.lightTheme}`} onClick={onToggleTheme} />
            <LanguageToggle lang={lang} label={t.language} onClick={onToggleLang} />
          </div>
          <div>
            <span className="section-kicker">{mode === "setup" ? t.setupFirst : t.signIn}</span>
            <h1>{mode === "setup" ? t.setupTitle : t.loginTitle}</h1>
          </div>
          <label>
            {t.username}
            <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" />
          </label>
          <label>
            {t.password}
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete={mode === "setup" ? "new-password" : "current-password"} />
          </label>
          {error && <div className="alert">{error}</div>}
          <button className="primary" type="submit">{mode === "setup" ? t.createAdmin : t.signIn}</button>
          <button className="link" type="button" onClick={() => setMode(mode === "login" ? "setup" : "login")}>
            {mode === "login" ? t.setupFirst : t.backToLogin}
          </button>
        </form>
      </div>
    </div>
  );
}

function RuntimeButton({ server, api, onDone }: { server: Server; api: ApiFn; onDone: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  async function setState(state: "running" | "stopped") {
    setBusy(true);
    try {
      await api(`/servers/${server.id}/runtime`, { method: "PUT", body: JSON.stringify({ state }) });
      await onDone();
    } finally {
      setBusy(false);
    }
  }
  return server.running ? (
    <button className="danger" disabled={busy} onClick={() => setState("stopped")}>
      <Square size={16} /> Stop
    </button>
  ) : (
    <button className="primary" disabled={busy} onClick={() => setState("running")}>
      <Power size={16} /> Start
    </button>
  );
}

type ApiFn = <T>(path: string, init?: RequestInit) => Promise<T>;

function CreateServer({ api, onDone, t, compact = false }: { api: ApiFn; onDone: () => Promise<void>; t: Copy; compact?: boolean }) {
  const [open, setOpen] = useState(!compact);
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [jar, setJar] = useState("server.jar");
  const [ram, setRam] = useState("2G");

  if (compact && !open) {
    return <button className="inline-add" onClick={() => setOpen(true)}><Plus size={16} /> {t.addAnother}</button>;
  }

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    await api("/servers", {
      method: "POST",
      body: JSON.stringify({
        name,
        slug,
        jar,
        javaPath: "java",
        jvmArgs: [`-Xms${ram}`, `-Xmx${ram}`],
        mcArgs: ["nogui"]
      })
    });
    setName("");
    setSlug("");
    setOpen(false);
    await onDone();
  }

  return (
    <form className="create-strip" onSubmit={submit}>
      <div className="form-intro">
        <strong>{t.formTitle}</strong>
        <span>{t.formHelp}</span>
      </div>
      <label>{t.serverName}<input value={name} onChange={(e) => setName(e.target.value)} placeholder="Survival SMP" /></label>
      <label>{t.folder}<input value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="survival" /></label>
      <label>Jar<input value={jar} onChange={(e) => setJar(e.target.value)} /></label>
      <label>{t.ram}<input value={ram} onChange={(e) => setRam(e.target.value)} /></label>
      <button className="primary"><Plus size={16} /> {t.create}</button>
    </form>
  );
}

function Console({ server, token, t }: { server: Server; token: string; t: Copy }) {
  const host = window.location.host;
  const protocol = window.location.protocol === "https:" ? "wss" : "ws";
  const terminalRef = useRef<HTMLDivElement | null>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const [status, setStatus] = useState(server.running ? t.consoleConnecting : t.consoleHistory);
  const [connected, setConnected] = useState(false);
  const [command, setCommand] = useState("");

  useEffect(() => {
    const node = terminalRef.current;
    if (!node) return;
    setConnected(false);
    const terminal = new Terminal({
      cursorBlink: false,
      disableStdin: true,
      fontFamily: "JetBrains Mono, Consolas, monospace",
      fontSize: 13
    });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(node);
    fit.fit();
    setStatus(t.consoleConnecting);
    const ws = new WebSocket(`${protocol}://${host}/api/v1/servers/${server.id}/console/ws?token=${encodeURIComponent(token)}`);
    socketRef.current = ws;
    ws.onmessage = (event) => terminal.write(event.data);
    ws.onopen = () => {
      setConnected(true);
      setStatus(server.running ? t.consoleLive : t.consoleHistory);
    };
    ws.onerror = () => {
      setStatus(t.consoleDisconnected);
      terminal.writeln("\r\nConsole connection error.\r\n");
    };
    ws.onclose = () => {
      setConnected(false);
      if (socketRef.current === ws) socketRef.current = null;
      setStatus(t.consoleDisconnected);
    };
    const resize = () => fit.fit();
    window.addEventListener("resize", resize);
    return () => {
      window.removeEventListener("resize", resize);
      setConnected(false);
      if (socketRef.current === ws) socketRef.current = null;
      ws.close();
      terminal.dispose();
    };
  }, [host, protocol, server.id, server.name, server.running, t, token]);

  function sendCommand(event: React.FormEvent) {
    event.preventDefault();
    const text = command.trim();
    const ws = socketRef.current;
    if (!text || !server.running || !connected || !ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(`${text}\n`);
    setCommand("");
  }

  const commandDisabled = !server.running || !connected;

  return (
    <section className="console-panel">
      <PanelHeader title={t.liveConsole} subtitle={`${server.name} - ${t.websocketSession}`} status={status} />
      <div className="terminal-frame" ref={terminalRef} />
      <form className="console-command" onSubmit={sendCommand}>
        <input
          value={command}
          onChange={(event) => setCommand(event.target.value)}
          placeholder={server.running ? t.consoleCommandPlaceholder : t.consoleStopped}
          disabled={commandDisabled}
          autoComplete="off"
        />
        <button className="primary" type="submit" disabled={commandDisabled || !command.trim()}>
          <Send size={15} /> {t.sendCommand}
        </button>
      </form>
    </section>
  );
}

function PanelHeader({ title, subtitle, status }: { title: string; subtitle: string; status?: string }) {
  return (
    <div className="panel-header">
      <div>
        <span className="section-kicker">{title}</span>
        <strong>{subtitle}</strong>
      </div>
      {status && <span className="panel-status">{status}</span>}
    </div>
  );
}

function Files({ server, api, token, t }: { server: Server; api: ApiFn; token: string; t: Copy }) {
  const [path, setPath] = useState(".");
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [editing, setEditing] = useState("");
  const [content, setContent] = useState("");

  async function load(nextPath = path) {
    const body = await api<{ path: string; entries: FileEntry[] }>(`/servers/${server.id}/files?path=${encodeURIComponent(nextPath)}`);
    setPath(body.path || ".");
    setEntries(body.entries);
  }

  useEffect(() => {
    load(".").catch(console.error);
  }, [server.id]);

  async function openFile(entry: FileEntry) {
    if (entry.dir) {
      await load(entry.path);
      return;
    }
    const body = await api<{ content: string }>(`/servers/${server.id}/files/content?path=${encodeURIComponent(entry.path)}`);
    setEditing(entry.path);
    setContent(body.content);
  }

  async function upload(file: File) {
    const form = new FormData();
    form.append("file", file);
    await api(`/servers/${server.id}/files/upload?path=${encodeURIComponent(path)}`, { method: "POST", body: form });
    await load();
  }

  return (
    <section className="files-grid">
      <div className="file-list">
        <div className="file-toolbar">
          <span>{path}</span>
          <label className="upload-button">
            <Upload size={15} /> {t.upload}
            <input type="file" onChange={(e) => e.target.files?.[0] && upload(e.target.files[0])} />
          </label>
        </div>
        <div className="file-scroll">
          <div className="file-columns">
            <span>{t.nameColumn}</span>
            <span>{t.sizeColumn}</span>
          </div>
          {path !== "." && <button className="file-row" onClick={() => load(path.split("/").slice(0, -1).join("/") || ".")}><Folder size={16} /> ..</button>}
          {entries.length === 0 && <p className="panel-empty">{t.emptyFolder}</p>}
          {entries.map((entry) => (
            <div className="file-row" key={entry.path}>
              <button className="file-open" onClick={() => openFile(entry)}>
                {entry.dir ? <Folder size={16} /> : <FileText size={16} />}
                <span>{entry.name}</span>
                <small>{entry.dir ? "folder" : formatBytes(entry.size)}</small>
              </button>
              {!entry.dir && (
                <a className="icon-link" href={`${apiBase}/servers/${server.id}/files/download?path=${encodeURIComponent(entry.path)}&token=${encodeURIComponent(token)}`} title="Download">
                  <Download size={15} />
                </a>
              )}
              <button
                className="icon-danger"
                title="Delete"
                onClick={async () => {
                  if (!confirm(`Delete ${entry.path}?`)) return;
                  await api(`/servers/${server.id}/files/content?path=${encodeURIComponent(entry.path)}`, { method: "DELETE" });
                  if (editing === entry.path) {
                    setEditing("");
                    setContent("");
                  }
                  await load();
                }}
              >
                <Trash2 size={15} />
              </button>
            </div>
          ))}
        </div>
      </div>
      <div className="editor">
        <div className="file-toolbar editor-toolbar">
          <div>
            <strong>{t.fileEditor}</strong>
            <span>{editing || t.chooseText}</span>
          </div>
          {editing && (
            <button
              onClick={async () => {
                await api(`/servers/${server.id}/files/content?path=${encodeURIComponent(editing)}`, { method: "PUT", body: JSON.stringify({ content }) });
                await load();
              }}
            >
              <Save size={15} /> {t.save}
            </button>
          )}
        </div>
        <textarea value={content} onChange={(e) => setContent(e.target.value)} spellCheck={false} />
      </div>
    </section>
  );
}

function MetricsView({ server, api, t }: { server: Server; api: ApiFn; t: Copy }) {
  const [metrics, setMetrics] = useState<Metrics | null>(null);
  const [samples, setSamples] = useState<MetricSample[]>([]);

  useEffect(() => {
    let alive = true;
    async function tick() {
      const body = await api<Metrics>(`/servers/${server.id}/metrics`);
      if (!alive) return;
      setMetrics(body);
      setSamples((current) => {
        const next = [
          ...current,
          {
            ts: body.sampledAt ? new Date(body.sampledAt).getTime() : Date.now(),
            cpu: body.process?.cpu || 0,
            rss: body.process?.rss || 0,
            disk: body.disk.bytes || 0,
            running: body.running
          }
        ];
        return next.slice(-60);
      });
    }
    setMetrics(null);
    setSamples([]);
    tick().catch(console.error);
    const timer = window.setInterval(() => tick().catch(console.error), 1000);
    return () => {
      alive = false;
      window.clearInterval(timer);
    };
  }, [server.id]);

  const rssMax = Math.max(512 * 1024 * 1024, ...samples.map((sample) => sample.rss));
  const cpuMax = Math.max(100, ...samples.map((sample) => sample.cpu), metrics?.process?.cpu || 0);
  const processLabel = metrics?.process
    ? `${metrics.process.name || t.jvmProcess} - PID ${metrics.process.pid}`
    : t.waitingForSamples;

  return (
    <section className="metrics-layout">
      <div className="metrics">
        <Metric icon={<Power size={18} />} label={t.status} value={metrics?.running ? t.running : t.stopped} progress={metrics?.running ? 100 : 8} />
        <Metric icon={<Activity size={18} />} label="CPU" value={`${metrics?.process?.cpu?.toFixed(1) || "0.0"}%`} progress={Math.min(metrics?.process?.cpu || 0, 100)} />
        <Metric icon={<HardDrive size={18} />} label={t.ramRss} value={formatBytes(metrics?.process?.rss || 0)} progress={Math.min(((metrics?.process?.rss || 0) / rssMax) * 100, 100)} />
        <Metric icon={<Folder size={18} />} label={t.folderSize} value={formatBytes(metrics?.disk.bytes || 0)} progress={Math.min(((metrics?.disk.bytes || 0) / (1024 * 1024 * 1024 * 10)) * 100, 100)} />
      </div>
      <div className="chart-panel">
        <div className="chart-panel-head">
          <div>
            <span>{t.processMonitor}</span>
            <strong>{processLabel}</strong>
          </div>
          <em>{t.samplingEverySecond}</em>
        </div>
        <div className="chart-grid">
          <MetricChart title="CPU" subtitle={metrics?.process?.cpuMode === "raw_multicore_tree" ? "Raw tree" : t.last60Seconds} samples={samples} valueKey="cpu" max={cpuMax} formatter={(value) => `${value.toFixed(1)}%`} />
          <MetricChart title={t.ramRss} subtitle={t.last60Seconds} samples={samples} valueKey="rss" max={rssMax} formatter={formatBytes} />
        </div>
      </div>
    </section>
  );
}

function MetricChart({
  title,
  subtitle,
  samples,
  valueKey,
  max,
  formatter
}: {
  title: string;
  subtitle: string;
  samples: MetricSample[];
  valueKey: "cpu" | "rss" | "disk";
  max: number;
  formatter: (value: number) => string;
}) {
  const width = 360;
  const height = 132;
  const padding = 12;
  const plotted = samples.filter((sample) => sample.running || sample[valueKey] > 0);
  const latestSample = samples.length ? samples[samples.length - 1] : undefined;
  const latest = latestSample?.[valueKey] || 0;
  const chartPoints = plotted.map((sample, index) => {
    const x = padding + (index / Math.max(plotted.length - 1, 1)) * (width - padding * 2);
    const y = height - padding - (Math.min(sample[valueKey], max) / Math.max(max, 1)) * (height - padding * 2);
    return { x, y };
  });
  const points = chartPoints.map((point) => `${point.x.toFixed(2)},${point.y.toFixed(2)}`).join(" ");
  const baseline = height - padding;
  const areaPath = chartPoints.length
    ? [
        `M ${chartPoints[0].x.toFixed(2)} ${baseline.toFixed(2)}`,
        ...chartPoints.map((point) => `L ${point.x.toFixed(2)} ${point.y.toFixed(2)}`),
        `L ${chartPoints[chartPoints.length - 1].x.toFixed(2)} ${baseline.toFixed(2)}`,
        "Z"
      ].join(" ")
    : "";
  const latestPoint = chartPoints[chartPoints.length - 1];

  return (
    <div className="metric-chart">
      <div className="chart-meta">
        <div>
          <span>{title}</span>
          <small>{subtitle}</small>
        </div>
        <strong>{formatter(latest)}</strong>
      </div>
      {plotted.length > 1 ? (
        <svg className="chart-svg" viewBox={`0 0 ${width} ${height}`} role="img" aria-label={`${title} chart`}>
          <path className="chart-grid-line" d={`M ${padding} ${height - padding} H ${width - padding}`} />
          <path className="chart-grid-line" d={`M ${padding} ${height * 0.66} H ${width - padding}`} />
          <path className="chart-grid-line" d={`M ${padding} ${height * 0.33} H ${width - padding}`} />
          <path className="chart-area" d={areaPath} />
          <polyline className="chart-line" points={points} />
          {latestPoint && <circle className="chart-point" cx={latestPoint.x} cy={latestPoint.y} r="4" />}
        </svg>
      ) : (
        <div className="chart-empty">{samples.length ? formatter(latest) : "No samples"}</div>
      )}
    </div>
  );
}

function Metric({ icon, label, value, progress }: { icon: React.ReactNode; label: string; value: string; progress: number }) {
  return (
    <div className="metric">
      <div className="metric-head">{icon}<span>{label}</span></div>
      <strong>{value}</strong>
      <div className="metric-bar"><span style={{ width: `${Math.max(4, Math.min(progress, 100))}%` }} /></div>
    </div>
  );
}

function UpdateWidget({ api, t }: { api: ApiFn; t: Copy }) {
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);

  async function load() {
    setBusy(true);
    setMessage("");
    try {
      const nextStatus = await api<UpdateStatus>("/update/status");
      setStatus(nextStatus);
      if (!nextStatus.available) {
        setMessage(t.noUpdateAvailable);
        return;
      }
      if (confirm(t.updateConfirm)) {
        await apply(nextStatus);
      }
    } catch (err) {
      setMessage(err instanceof Error ? err.message : "Update check failed");
    } finally {
      setBusy(false);
    }
  }

  async function apply(nextStatus: UpdateStatus) {
    if (!nextStatus.available) {
      setMessage(t.noUpdateAvailable);
      return;
    }
    const body = await api<Record<string, unknown>>("/update/apply", { method: "POST", body: "{}" });
    setMessage(JSON.stringify(body));
  }

  useEffect(() => {
    api<UpdateStatus>("/update/status").then(setStatus).catch((err) => setMessage(err.message));
  }, []);

  const releasesUrl = status?.releaseUrl || "https://github.com/rekis-0103/MyPanel/releases";

  return (
    <section className="update-card">
      <div className="update-card-head">
        <span className="section-kicker">{t.update}</span>
        <strong>{status?.latest || t.selfUpdate}</strong>
      </div>
      <div className={`update-state ${status?.available ? "available" : ""}`}>
        {status?.available ? t.updateAvailable : t.noUpdateAvailable}
      </div>
      <div className="update-actions">
        <button onClick={load} disabled={busy}>
          <RefreshCcw size={15} /> {t.checkUpdate}
        </button>
        <a className="update-link" href={releasesUrl} target="_blank" rel="noreferrer" title={t.openReleases}>
          <ExternalLink size={15} /> {t.openReleases}
        </a>
      </div>
      {message && <p className="update-message">{message}</p>}
    </section>
  );
}

type UpdateStatus = {
  configured?: boolean;
  current?: string;
  latest?: string;
  releaseUrl?: string;
  assetUrl?: string;
  available?: boolean;
  repo?: string;
};

function formatBytes(value: number) {
  if (!value) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit += 1;
  }
  return `${size.toFixed(size >= 10 || unit === 0 ? 0 : 1)} ${units[unit]}`;
}

ReactDOM.createRoot(document.getElementById("root")!).render(<App />);
