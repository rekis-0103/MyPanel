# MyPanel

Portable Minecraft server panel for Windows. The release artifact is a single `panel.exe` that embeds the React frontend and creates its runtime folders beside the executable.

## Client Folder Layout

After running `panel.exe`, the client laptop folder becomes:

```text
MyPanel/
  panel.exe
  data/
    config.json
    users.json
    servers.json
    logs/
    updates/
  server/
    survival/
    creative/
    modded-smp/
```

Every Minecraft server must live under `server/<slug>/`. The panel stores metadata in `data/servers.json`, while the actual Minecraft files stay in the server folder.

## Build

Requirements are only needed on the developer laptop:

- Go 1.22+
- Node.js 22+

```powershell
npm install --prefix web
npm run build
```

The output is:

```text
dist/MyPanel/panel.exe
```

Copy `dist/MyPanel/panel.exe` to any Windows folder and run it. The client laptop does not need Go, Node.js, or Git. Java must be installed separately for Minecraft.

## First Run

Open:

```text
http://127.0.0.1:8080
```

Use `First run setup` to create the first admin account. For tunnel/public access, expose port `8080` through your tunnel or change `data/config.json`.

## Release

Push a tag like `v0.1.0`. GitHub Actions builds the frontend, embeds it into Go, builds `panel.exe`, creates `panel.exe.sha256`, and uploads both files to the GitHub Release.
