import React, { useEffect, useMemo, useRef, useState } from "react";
import ReactDOM from "react-dom/client";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import {
  Activity,
  Download,
  FileText,
  Folder,
  HardDrive,
  LogOut,
  Plus,
  Power,
  RefreshCcw,
  Save,
  Server as ServerIcon,
  Shield,
  Square,
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
  disk: { bytes: number };
  process?: { pid: number; cpu: number; rss: number; children: number[] };
};

const apiBase = "/api/v1";

function App() {
  const [token, setToken] = useState(() => localStorage.getItem("mypanel.token") || "");
  const [user, setUser] = useState<User | null>(null);
  const [servers, setServers] = useState<Server[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [view, setView] = useState<"console" | "files" | "metrics" | "update">("console");
  const [error, setError] = useState("");

  const selected = servers.find((server) => server.id === selectedId) || servers[0];

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
    return <AuthScreen onToken={loginDone} />;
  }

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <div className="cube-mark">MP</div>
          <div>
            <strong>MyPanel</strong>
            <span>Portable Minecraft control</span>
          </div>
        </div>

        <button className="new-server" onClick={() => setView("console")}>
          <Plus size={16} /> Server baru
        </button>

        <div className="server-list">
          {servers.map((server) => (
            <button
              key={server.id}
              className={`server-row ${selected?.id === server.id ? "active" : ""}`}
              onClick={() => setSelectedId(server.id)}
            >
              <ServerIcon size={17} />
              <span>{server.name}</span>
              <i className={server.running ? "dot on" : "dot"} />
            </button>
          ))}
          {servers.length === 0 && <p className="empty">Belum ada server di folder server/.</p>}
        </div>

        <div className="identity">
          <Shield size={16} />
          <span>{user.username}</span>
          <b>{user.role}</b>
          <button onClick={logout} title="Logout">
            <LogOut size={16} />
          </button>
        </div>
      </aside>

      <main className="workspace">
        <header className="topbar">
          <div>
            <span className="eyebrow">server/{selected?.slug || "new"}</span>
            <h1>{selected?.name || "Tambah Minecraft Server"}</h1>
          </div>
          <div className="actions">
            <button onClick={() => refresh().catch((err) => setError(err.message))}>
              <RefreshCcw size={16} /> Refresh
            </button>
            {selected && (
              <RuntimeButton server={selected} api={api} onDone={refresh} />
            )}
          </div>
        </header>

        {error && <div className="alert">{error}</div>}

        {!selected ? (
          <CreateServer api={api} onDone={refresh} />
        ) : (
          <>
            <nav className="tabs">
              <button className={view === "console" ? "active" : ""} onClick={() => setView("console")}>
                <TerminalSquare size={16} /> Console
              </button>
              <button className={view === "files" ? "active" : ""} onClick={() => setView("files")}>
                <Folder size={16} /> Files
              </button>
              <button className={view === "metrics" ? "active" : ""} onClick={() => setView("metrics")}>
                <Activity size={16} /> Metrics
              </button>
              {user.role === "admin" && (
                <button className={view === "update" ? "active" : ""} onClick={() => setView("update")}>
                  <Download size={16} /> Update
                </button>
              )}
            </nav>

            {view === "console" && <Console server={selected} token={token} />}
            {view === "files" && <Files server={selected} api={api} token={token} />}
            {view === "metrics" && <MetricsView server={selected} api={api} />}
            {view === "update" && <UpdateView api={api} />}

            <CreateServer compact api={api} onDone={refresh} />
          </>
        )}
      </main>
    </div>
  );
}

function AuthScreen({ onToken }: { onToken: (token: string) => void }) {
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
      <form className="auth-panel" onSubmit={submit}>
        <div className="brand large">
          <div className="cube-mark">MP</div>
          <div>
            <strong>MyPanel</strong>
            <span>Portable server panel</span>
          </div>
        </div>
        <h1>{mode === "setup" ? "Buat admin pertama" : "Masuk ke panel"}</h1>
        <label>
          Username
          <input value={username} onChange={(e) => setUsername(e.target.value)} />
        </label>
        <label>
          Password
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </label>
        {error && <div className="alert">{error}</div>}
        <button className="primary" type="submit">{mode === "setup" ? "Create admin" : "Login"}</button>
        <button className="link" type="button" onClick={() => setMode(mode === "login" ? "setup" : "login")}>
          {mode === "login" ? "First run setup" : "Back to login"}
        </button>
      </form>
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

function CreateServer({ api, onDone, compact = false }: { api: ApiFn; onDone: () => Promise<void>; compact?: boolean }) {
  const [open, setOpen] = useState(!compact);
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [jar, setJar] = useState("server.jar");
  const [ram, setRam] = useState("2G");

  if (compact && !open) {
    return <button className="inline-add" onClick={() => setOpen(true)}><Plus size={16} /> Tambah server</button>;
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
      <label>Nama server<input value={name} onChange={(e) => setName(e.target.value)} placeholder="Survival SMP" /></label>
      <label>Folder<input value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="survival" /></label>
      <label>Jar<input value={jar} onChange={(e) => setJar(e.target.value)} /></label>
      <label>RAM<input value={ram} onChange={(e) => setRam(e.target.value)} /></label>
      <button className="primary"><Plus size={16} /> Buat</button>
    </form>
  );
}

function Console({ server, token }: { server: Server; token: string }) {
  const host = window.location.host;
  const protocol = window.location.protocol === "https:" ? "wss" : "ws";
  const terminalRef = useRef<HTMLDivElement | null>(null);
  const terminal = useMemo(() => new Terminal({ cursorBlink: true, fontFamily: "JetBrains Mono, Consolas, monospace", fontSize: 13 }), [server.id]);

  useEffect(() => {
    const node = terminalRef.current;
    if (!node) return;
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(node);
    fit.fit();
    const ws = new WebSocket(`${protocol}://${host}/api/v1/servers/${server.id}/console/ws?token=${encodeURIComponent(token)}`);
    ws.onmessage = (event) => terminal.write(event.data);
    ws.onopen = () => terminal.writeln(`\r\nConnected to ${server.name}`);
    terminal.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(data);
    });
    const resize = () => fit.fit();
    window.addEventListener("resize", resize);
    return () => {
      window.removeEventListener("resize", resize);
      ws.close();
      terminal.dispose();
    };
  }, [server.id, token]);

  return <div className="terminal-frame" ref={terminalRef} />;
}

function Files({ server, api, token }: { server: Server; api: ApiFn; token: string }) {
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
            <Upload size={15} /> Upload
            <input type="file" onChange={(e) => e.target.files?.[0] && upload(e.target.files[0])} />
          </label>
        </div>
        {path !== "." && <button className="file-row" onClick={() => load(path.split("/").slice(0, -1).join("/") || ".")}><Folder size={16} /> ..</button>}
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
      <div className="editor">
        <div className="file-toolbar">
          <span>{editing || "Pilih file teks"}</span>
          {editing && (
            <button
              onClick={async () => {
                await api(`/servers/${server.id}/files/content?path=${encodeURIComponent(editing)}`, { method: "PUT", body: JSON.stringify({ content }) });
                await load();
              }}
            >
              <Save size={15} /> Save
            </button>
          )}
        </div>
        <textarea value={content} onChange={(e) => setContent(e.target.value)} spellCheck={false} />
      </div>
    </section>
  );
}

function MetricsView({ server, api }: { server: Server; api: ApiFn }) {
  const [metrics, setMetrics] = useState<Metrics | null>(null);

  useEffect(() => {
    let alive = true;
    async function tick() {
      const body = await api<Metrics>(`/servers/${server.id}/metrics`);
      if (alive) setMetrics(body);
    }
    tick().catch(console.error);
    const timer = window.setInterval(() => tick().catch(console.error), 3000);
    return () => {
      alive = false;
      window.clearInterval(timer);
    };
  }, [server.id]);

  return (
    <section className="metrics">
      <Metric icon={<Power size={18} />} label="Status" value={metrics?.running ? "Running" : "Stopped"} />
      <Metric icon={<Activity size={18} />} label="CPU" value={`${metrics?.process?.cpu?.toFixed(1) || "0.0"}%`} />
      <Metric icon={<HardDrive size={18} />} label="RAM RSS" value={formatBytes(metrics?.process?.rss || 0)} />
      <Metric icon={<Folder size={18} />} label="Disk folder" value={formatBytes(metrics?.disk.bytes || 0)} />
    </section>
  );
}

function Metric({ icon, label, value }: { icon: React.ReactNode; label: string; value: string }) {
  return (
    <div className="metric">
      {icon}
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function UpdateView({ api }: { api: ApiFn }) {
  const [status, setStatus] = useState<Record<string, unknown> | null>(null);
  const [repo, setRepo] = useState("");
  const [message, setMessage] = useState("");
  async function load() {
    const [cfg, nextStatus] = await Promise.all([
      api<{ githubRepo: string }>("/config"),
      api<Record<string, unknown>>("/update/status")
    ]);
    setRepo(cfg.githubRepo || "");
    setStatus(nextStatus);
  }
  useEffect(() => {
    load().catch((err) => setMessage(err.message));
  }, []);
  return (
    <section className="update-panel">
      <h2>Self-update</h2>
      <label>
        GitHub repository
        <input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="owner/repo" />
      </label>
      <button
        onClick={async () => {
          await api("/config", { method: "PATCH", body: JSON.stringify({ githubRepo: repo }) });
          await load();
        }}
      >
        <Save size={16} /> Save repo
      </button>
      <pre>{JSON.stringify(status, null, 2)}</pre>
      {message && <div className="alert">{message}</div>}
      <button
        className="primary"
        onClick={async () => {
          const body = await api<Record<string, unknown>>("/update/apply", { method: "POST", body: "{}" });
          setMessage(JSON.stringify(body));
        }}
      >
        <Download size={16} /> Download dan replace panel.exe
      </button>
    </section>
  );
}

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
