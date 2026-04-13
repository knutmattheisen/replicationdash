# ReplicationDash v0.2.6 — SQL Server Replication Dashboard

Portable, zero-install DBA tool for near real-time monitoring of SQL Server Merge Replication.
Single Windows executable. No Node.js, Python, .NET runtime or installer required.

by **DBo**

---

## Features

* **Topology visualization** — automatic discovery of Publisher, Distributor and Subscribers with visual pipeline display
* **Bidirectional arrows** — separate Upload/Download status indicators per connection
* **Status cards per publication/subscriber** — instant overview with color-coded health (green/yellow/red), click for deep-dive
* **Card detail panel** — click any card to see filtered sessions, conflicts, blocking and an automatic action recommendation for that specific connection
* **Smart conflict thresholds** — under 6 conflicts = normal (green), 6–20 = warning (yellow), 20+ = critical (red)
* **Root cause classification** — automatic analysis: ⚙️ Systemisch (hardware/network) vs 👤 Nutzungsspezifisch (user error/config) with explanation
* **Health Ampel** — green/yellow/red with mouseover tooltip explaining current status
* **Merge Session monitoring** — MSmerge_sessions with duration, delivery rates, upload/download counts, error tracking
* **Conflict detection** — counts and displays conflicts from all MSmerge_conflict_* tables
* **Blocking chain analysis** — blocked/blocking sessions with SQL text, filtered for replication agents (replmerg.exe)
* **Troubleshooting system** — click on warnings for slide-in panel with problem description, causes and solutions
* **Mock mode** — fixed topology with realistic demo data, no real SQL Server needed
* **Sortable columns** — click any column header to sort ascending/descending
* **Resizable columns** — drag column header edges to resize
* **Copy to clipboard** — click any table cell to copy its value
* **Auto-refresh** — configurable 5s/10s/30s/60s/Pause
* **Dark mode** — professional dark theme
* **TLS encrypted connections** — all SQL Server connections use Encrypt=true
* **Server management via UI** — add, switch and remove SQL Server targets directly in the dashboard

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

1. Download `replicationdash.exe` from GitHub Releases
2. Place it in any folder on your admin PC
3. Double-click `replicationdash.exe`
4. Open browser: **http://localhost:9090**
5. Click **+ Add Server** to connect your first SQL Server instance

Connection settings are saved automatically to `servers.json` in the same folder.

---

## Mock / Demo Mode

To test ReplicationDash without a real SQL Server:

1. Click **+ Add Server**
2. Set **Authentifizierung** to **Mock (Demo)**
3. Give it any name (e.g. "Demo Server")
4. Click **Verbinden**

The mock mode uses a fixed topology (1 Publisher, 2 Subscribers, 4 Publications) and generates realistic scenarios including failed sessions, conflicts, and blocking chains.

A default mock server is created automatically on first start.

---

## What ReplicationDash Monitors

| Area              | Source                                              |
|------------------|------------------------------------------------------|
| Topology         | `MSpublications` + `MSsubscriptions` (distribution DB) |
| Merge Sessions   | `MSmerge_sessions` + `MSmerge_history` (distribution DB) |
| Conflicts        | `MSmerge_conflict_*` tables (publication DB)          |
| Blocking         | `sys.dm_exec_requests` + `sys.dm_exec_sessions` (master) |

**Time window:** Last 60 minutes. Publisher + Distributor are assumed to be on the same server.

---

## Root Cause Classification

ReplicationDash automatically analyzes problems and classifies them:

| Icon | Type | Examples |
|------|------|----------|
| ⚙️ | **Systemisch** | I/O bottleneck, network latency, timeout, server overload |
| 👤 | **Nutzungsspezifisch** | SSMS without COMMIT, ETL job blocking, RBAR updates, missing data ownership |

The classification is shown in the header and explains *why* there is a problem and *what to do about it*.

---

## Permissions (minimum required)

```sql
GRANT VIEW SERVER STATE TO [your_login];

USE [distribution];
GRANT SELECT ON MSmerge_sessions TO [your_login];
GRANT SELECT ON MSmerge_agents TO [your_login];
GRANT SELECT ON MSmerge_history TO [your_login];
GRANT SELECT ON MSpublications TO [your_login];
GRANT SELECT ON MSsubscriptions TO [your_login];

USE [YourPublicationDB];
GRANT SELECT ON SCHEMA::dbo TO [your_login];
```

---

## Building from source

Requirements: Go 1.21 or later

**Windows:**
```
build.bat
```

**Manual:**
```
go mod tidy
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -H windowsgui" -o replicationdash.exe .
```

---

## Architecture

```
[Browser] ──── HTTP ──── [replicationdash.exe :9090] ──── TLS/SQL ──── [SQL Server]
                          │
                          ├─ Serves HTML/CSS/JS (embedded in binary)
                          ├─ /api/* endpoints
                          └─ servers.json (auto-managed config)
```

---

## Changelog

### v0.2.3
* **Card detail panel** — click any status card for a slide-in with filtered sessions, conflicts, blocking and automatic action recommendation
* Smart conflict thresholds: <6 = normal, 6–20 = yellow, 20+ = red
* Status badge on cards now matches card color level consistently

### v0.2.1
* Star topology layout (Publisher centered on top, Subscribers below)
* Card color fix: blocking no longer turns all cards red globally
* Global health ampel now reflects worst card status
* Conflict thresholds implemented

### v0.2.0
* Renamed from MergeDash to **ReplicationDash by DBo**
* Topology visualization with automatic discovery
* Bidirectional Upload/Download arrows per connection
* Status cards computed server-side with fixed topology
* Root cause classification (Systemisch vs Nutzungsspezifisch)
* Fixed mock mode with stable topology (no more random card count)
* Collapsible detail sections
* Version number consistent across all files

### v0.1.x
* Initial release as MergeDash
* Basic session, conflict and blocking monitoring
* Mock mode, troubleshooting panel, dark theme

---

## Credits

Built by Knut Mattheisen · powered by Claude AI
