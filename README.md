# MergeDash v1.0 — SQL Server Merge Replication Dashboard

Portable, zero-install DBA tool for near real-time monitoring of bidirectional MS SQL Server Merge Replication.
Single Windows executable. No Node.js, Python, .NET runtime or installer required.

---

## Features

* **Server management via UI** — add, switch and remove SQL Server targets directly in the dashboard
* **Merge Session monitoring** — MSmerge_sessions with duration, delivery rates, upload/download counts, error tracking
* **Conflict detection** — counts and displays conflicts from all MSmerge_conflict_* tables
* **Blocking chain analysis** — blocked/blocking sessions with SQL text, filtered for replication agents (replmerg.exe)
* **Health Ampel** — green/yellow/red with mouseover tooltip explaining current status and reasons
* **Troubleshooting system** — click on warnings for slide-in panel with problem description, causes and solutions
* **Mock mode** — realistic demo data without real SQL Server connection
* **Sortable columns** — click any column header to sort ascending/descending
* **Resizable columns** — drag column header edges to resize
* **Copy to clipboard** — click any table cell to copy its value
* **Auto-refresh** — configurable 5s/10s/30s/60s/Pause
* **Dark mode** — professional dark theme
* **TLS encrypted connections** — all SQL Server connections use encrypted transport (Encrypt=true)

---

## Requirements

| Component       | Requirement                                              |
|----------------|----------------------------------------------------------|
| Your PC        | Windows, no runtime needed after build                    |
| SQL Server     | 2012+ with Merge Replication configured                   |
| Permissions    | `VIEW SERVER STATE` + access to distribution/publication DB |
| Windows Auth   | Domain-joined machine running under a domain account      |
| SQL Auth       | Valid SQL login with appropriate permissions               |

---

## Deployment

1. Download `mergedash.exe` from GitHub Releases
2. Place `mergedash.exe` in any folder on your admin PC
3. Double-click `mergedash.exe`
4. Open browser: **http://localhost:9090**
5. Click **+ Add Server** to connect your first SQL Server instance

Connection settings are saved automatically to `servers.json` in the same folder.

---

## Mock / Demo Mode

To test MergeDash without a real SQL Server:

1. Click **+ Add Server**
2. Set **Authentifizierung** to **Mock (Demo)**
3. Give it any name (e.g. "Demo Server")
4. Click **Verbinden**

The mock mode generates realistic test data including:
- Sessions with mixed statuses (Succeeded, Failed, Retry, InProgress)
- High latency scenarios (> 5 minutes)
- Merge conflicts (Update, Delete, Insert-Unique)
- Blocking chains where replmerg.exe is blocked by developer tools

A default mock server is created automatically on first start.

---

## What MergeDash Monitors

| Area              | Source                                              |
|------------------|------------------------------------------------------|
| Merge Sessions   | `MSmerge_sessions` + `MSmerge_history` (distribution DB) |
| Conflicts        | `MSmerge_conflict_*` tables (publication DB)          |
| Blocking         | `sys.dm_exec_requests` + `sys.dm_exec_sessions` (master) |

**Time window:** Last 60 minutes only. MergeDash focuses on the current state, not historical trends.

**Publisher + Distributor** are assumed to be on the same server.

---

## Permissions (minimum required)

```sql
-- On the monitored SQL Server instance:
GRANT VIEW SERVER STATE TO [your_login];

-- On the distribution database:
USE [distribution];
GRANT SELECT ON MSmerge_sessions TO [your_login];
GRANT SELECT ON MSmerge_agents TO [your_login];
GRANT SELECT ON MSmerge_history TO [your_login];
GRANT SELECT ON MSpublications TO [your_login];
GRANT SELECT ON MSsubscriptions TO [your_login];

-- On the publication database:
USE [YourPublicationDB];
GRANT SELECT ON SCHEMA::dbo TO [your_login]; -- for MSmerge_conflict_* tables
```

---

## servers.json (auto-managed)

The file is written automatically by the UI. You can also edit it manually:

```json
{
  "listen_port": 9090,
  "refresh_seconds": 10,
  "default_server": "PROD-SQL01",
  "servers": [
    {
      "name": "PROD-SQL01",
      "host": "PROD-SQL01.domain.local",
      "port": 1433,
      "instance": "",
      "auth_mode": "windows",
      "distribution_db": "distribution",
      "publication_db": "SalesDB"
    },
    {
      "name": "DEV-SQL01",
      "host": "192.168.1.50",
      "port": 1433,
      "instance": "",
      "auth_mode": "sql",
      "user": "mergedash_reader",
      "password": "YourPassword",
      "distribution_db": "distribution",
      "publication_db": "SalesDB"
    },
    {
      "name": "Demo (Mock)",
      "host": "localhost",
      "port": 1433,
      "auth_mode": "mock",
      "distribution_db": "distribution",
      "publication_db": "AdventureWorks"
    }
  ]
}
```

| Field            | Values                    | Notes                              |
|-----------------|---------------------------|------------------------------------|
| auth_mode       | `"windows"`, `"sql"`, `"mock"` | Mock generates test data        |
| instance        | `""` or `"INST_NAME"`     | Named instance, blank = default    |
| distribution_db | `"distribution"`          | Name of the distribution database  |
| publication_db  | `"YourDB"`                | Name of the published database     |

---

## Health Ampel Logic

| Level  | Conditions                                                  |
|--------|-------------------------------------------------------------|
| 🟢 Green  | All sessions normal, no blocking, < 5 conflicts          |
| 🟡 Yellow | High latency (> 5 min), > 5 conflicts, no recent sessions |
| 🔴 Red    | Failed sessions, replication agent blocked                 |

Hover over the ampel to see the exact reasons.

---

## Troubleshooting System

Click on highlighted cells (yellow = warning, red = critical) to open the troubleshooting panel:

| Trigger                | Opens                              |
|-----------------------|------------------------------------|
| Duration > 300s       | "Hohe Merge-Latenz"               |
| Error count > 0       | "Merge-Session fehlgeschlagen"     |
| Update conflict       | "Update-Konflikte"                 |
| Delete conflict       | "Delete-Konflikte"                 |
| Repl agent blocked    | "Replication Agent wird blockiert" |

Each entry contains: problem description, possible causes, and concrete solutions.

---

## Building from source

Requirements: Go 1.21 or later

**Windows:**
```
build.bat
```

**Linux / WSL (cross-compile for Windows):**
```
chmod +x build.sh
./build.sh
```

**Manual:**
```
go mod tidy
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -H windowsgui" -o mergedash.exe .
```

---

## Architecture

```
[Browser] ──── HTTP ──── [mergedash.exe :9090] ──── TLS/SQL ──── [SQL Server]
                          │
                          ├─ Serves HTML/CSS/JS (embedded in binary)
                          ├─ /api/* endpoints
                          └─ servers.json (auto-managed config)
```

The executable embeds all frontend assets. Only `mergedash.exe` needed to run.
`servers.json` is created automatically on first server connection.

---

## What MergeDash is NOT

* Not a historical trend database — monitors the last 60 minutes only
* Not a replacement for Replication Monitor in SSMS — focuses on key metrics and blocking
* Not designed for Transactional or Snapshot replication — Merge Replication only
* Not a production monitoring service — built for DBA admin workstations

---

## Credits

Built by Knut Mattheisen · powered by Claude AI
