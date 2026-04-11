package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

//go:embed static/*
var staticFiles embed.FS

// ───────────────────────────────────────────────────────────────
// Topology Discovery Queries
// ───────────────────────────────────────────────────────────────

const queryTopology = `
SELECT
    CAST(SERVERPROPERTY('ServerName') AS NVARCHAR(256)) AS server_name,
    p.publication                     AS publication_name,
    p.publisher_db                    AS publisher_db,
    ISNULL(sub.subscriber_server, '') AS subscriber_server,
    ISNULL(sub.db_name, '')           AS subscriber_db,
    CASE
        WHEN sub.subscription_type = 0 THEN 'Push'
        WHEN sub.subscription_type = 1 THEN 'Pull'
        ELSE 'Unknown'
    END                               AS subscription_type
FROM dbo.MSpublications p
LEFT JOIN dbo.MSsubscriptions sub
    ON p.publisher_db = sub.publisher_db
   AND p.publication = sub.publication
ORDER BY p.publication, sub.subscriber_server;
`

const queryLatencySessions = `
SELECT
    p.name                          AS publication_name,
    ISNULL(sub.subscriber_server, '')  AS subscriber,
    ISNULL(sub.db_name, '')         AS subscriber_db,
    s.session_id,
    s.agent_id,
    s.start_time,
    ISNULL(s.end_time, GETDATE())   AS end_time,
    s.duration                      AS duration_seconds,
    s.delivery_rate,
    s.upload_inserts,
    s.upload_updates,
    s.upload_deletes,
    s.download_inserts,
    s.download_updates,
    s.download_deletes,
    s.schema_changes,
    s.errors_count                  AS error_count,
    s.run_status,
    CASE s.run_status
        WHEN 1 THEN 'Started'
        WHEN 2 THEN 'Succeeded'
        WHEN 3 THEN 'InProgress'
        WHEN 4 THEN 'Idle'
        WHEN 5 THEN 'Retry'
        WHEN 6 THEN 'Failed'
        ELSE 'Unknown(' + CAST(s.run_status AS VARCHAR) + ')'
    END                             AS run_status_text,
    ISNULL(h.comments, '')          AS last_message
FROM MSmerge_sessions s
INNER JOIN MSmerge_agents a
    ON s.agent_id = a.id
INNER JOIN dbo.MSpublications p
    ON a.publisher_db = p.publisher_db
   AND a.publication = p.publication
LEFT JOIN dbo.MSsubscriptions sub
    ON a.publisher_db = sub.publisher_db
   AND a.publication = sub.publication
   AND sub.subscription_type = 0
OUTER APPLY (
    SELECT TOP 1 comments
    FROM MSmerge_history mh
    WHERE mh.session_id = s.session_id
    ORDER BY mh.time DESC
) h
WHERE s.start_time >= DATEADD(MINUTE, -60, GETDATE())
ORDER BY s.start_time DESC;
`

const queryConflicts = `
DECLARE @sql NVARCHAR(MAX) = N'';

SELECT @sql = @sql +
    'SELECT ''' + REPLACE(ct.name, '''', '''''') + ''' AS conflict_table, ' +
    '       ci.tablenick, ' +
    '       ci.rowguid, ' +
    '       ci.origin_datasource, ' +
    '       ci.conflict_type, ' +
    '       CASE ci.conflict_type ' +
    '           WHEN 1  THEN ''Update'' ' +
    '           WHEN 2  THEN ''Insert-Insert'' ' +
    '           WHEN 3  THEN ''Upload-Update/Delete'' ' +
    '           WHEN 4  THEN ''Download-Update/Delete'' ' +
    '           WHEN 5  THEN ''Upload-Insert'' ' +
    '           WHEN 6  THEN ''Download-Insert'' ' +
    '           WHEN 7  THEN ''Upload-Delete'' ' +
    '           WHEN 8  THEN ''Download-Delete'' ' +
    '           ELSE ''Other('' + CAST(ci.conflict_type AS VARCHAR) + '')'' ' +
    '       END AS conflict_type_text, ' +
    '       ci.reason_code, ' +
    '       ci.reason_text, ' +
    '       ci.pubid, ' +
    '       ci.create_time ' +
    'FROM ' + QUOTENAME(ct.name) + ' ci ' +
    'UNION ALL '
FROM sys.tables ct
WHERE ct.name LIKE 'MSmerge_conflict_%'
  AND ct.name <> 'MSmerge_conflicts_info';

IF LEN(@sql) > 10
BEGIN
    SET @sql = LEFT(@sql, LEN(@sql) - 10);
    EXEC sp_executesql @sql;
END
ELSE
BEGIN
    SELECT NULL AS conflict_table, NULL AS tablenick, NULL AS rowguid,
           NULL AS origin_datasource, NULL AS conflict_type,
           NULL AS conflict_type_text, NULL AS reason_code,
           NULL AS reason_text, NULL AS pubid, NULL AS create_time
    WHERE 1 = 0;
END
`

const queryBlocking = `
WITH BlockingChain AS (
    SELECT
        r.session_id                        AS blocked_spid,
        r.blocking_session_id               AS blocking_spid,
        r.wait_type,
        r.wait_time                         AS wait_time_ms,
        r.wait_resource,
        r.command,
        r.status                            AS request_status,
        DB_NAME(r.database_id)              AS database_name,
        ISNULL(SUBSTRING(t.text, 1, 4000), '')  AS blocked_sql_text,
        ISNULL(s.program_name, '')          AS blocked_program,
        ISNULL(s.host_name, '')             AS blocked_host,
        ISNULL(s.login_name, '')            AS blocked_login
    FROM sys.dm_exec_requests r
    INNER JOIN sys.dm_exec_sessions s
        ON r.session_id = s.session_id
    OUTER APPLY sys.dm_exec_sql_text(r.sql_handle) t
    WHERE r.blocking_session_id > 0
),
BlockerInfo AS (
    SELECT
        s.session_id                        AS blocker_spid,
        ISNULL(s.program_name, '')          AS blocker_program,
        ISNULL(s.host_name, '')             AS blocker_host,
        ISNULL(s.login_name, '')            AS blocker_login,
        s.status                            AS blocker_status,
        c.most_recent_sql_handle,
        ISNULL(SUBSTRING(t.text, 1, 4000), '') AS blocker_sql_text
    FROM sys.dm_exec_sessions s
    LEFT JOIN sys.dm_exec_connections c
        ON s.session_id = c.session_id
    OUTER APPLY sys.dm_exec_sql_text(c.most_recent_sql_handle) t
    WHERE s.session_id IN (
        SELECT DISTINCT blocking_spid FROM BlockingChain
    )
)
SELECT
    bc.blocked_spid,
    bc.blocking_spid,
    ISNULL(bc.wait_type, '')    AS wait_type,
    bc.wait_time_ms,
    ISNULL(bc.wait_resource, '') AS wait_resource,
    ISNULL(bc.command, '')      AS command,
    bc.request_status,
    ISNULL(bc.database_name, '') AS database_name,
    bc.blocked_sql_text,
    bc.blocked_program,
    bc.blocked_host,
    bc.blocked_login,
    ISNULL(bi.blocker_program, '')  AS blocker_program,
    ISNULL(bi.blocker_host, '')    AS blocker_host,
    ISNULL(bi.blocker_login, '')   AS blocker_login,
    ISNULL(bi.blocker_status, '')  AS blocker_status,
    ISNULL(bi.blocker_sql_text, '') AS blocker_sql_text,
    CASE
        WHEN bc.blocked_program LIKE '%replmerg%'
          OR bc.blocked_program LIKE '%REPL-Merge%' THEN 1
        WHEN ISNULL(bi.blocker_program, '') LIKE '%replmerg%'
          OR ISNULL(bi.blocker_program, '') LIKE '%REPL-Merge%' THEN 1
        ELSE 0
    END AS replication_related
FROM BlockingChain bc
LEFT JOIN BlockerInfo bi
    ON bc.blocking_spid = bi.blocker_spid
ORDER BY bc.wait_time_ms DESC;
`

// ───────────────────────────────────────────────────────────────
// Troubleshooting Dictionary
// ───────────────────────────────────────────────────────────────

type TroubleshootingEntry struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Trigger   string   `json:"trigger"`
	Problem   string   `json:"problem"`
	Causes    []string `json:"causes"`
	Solutions []string `json:"solutions"`
}

var troubleshootingData = []TroubleshootingEntry{
	{
		ID: "high_latency", Title: "Hohe Merge-Latenz", Trigger: "latency_high",
		Problem: "Die Merge-Session dauert deutlich länger als erwartet (> 5 Minuten).",
		Causes: []string{
			"Große Transaktionsmengen durch Batch-Jobs",
			"Netzwerk-Engpässe zwischen Publisher und Subscriber",
			"Fehlende oder fragmentierte Indizes auf replizierten Tabellen",
			"Hohe Server-Auslastung (CPU / I/O)",
		},
		Solutions: []string{
			"Prüfe Upload-/Download-Counts – bei auffällig hohen Zahlen nach Massen-Operationen suchen.",
			"Netzwerkdurchsatz prüfen mit iperf3.",
			"Index-Maintenance: ALTER INDEX ALL ON <table> REBUILD;",
			"sys.dm_os_wait_stats nach Top-Waits prüfen.",
			"Merge-Agent-Profil auf 'High Volume Server-to-Server' umstellen.",
		},
	},
	{
		ID: "conflicts_update", Title: "Update-Konflikte", Trigger: "conflict_update",
		Problem: "Dieselbe Zeile wurde auf Publisher und Subscriber gleichzeitig geändert.",
		Causes: []string{
			"Keine klare Datenhoheit – beide Seiten ändern dieselbe Tabelle.",
			"Fehlende Partitionierung der Datenverantwortung.",
		},
		Solutions: []string{
			"Klare Datenhoheit implementieren: Subscriber ändern nur 'ihre' Datensätze.",
			"Custom Conflict Resolver nutzen (Prioritäts- oder Zeitstempel-basiert).",
			"Prüfen ob die App RBAR statt Batch-Updates sendet.",
			"Column-Level Tracking aktivieren: sp_changemergearticle @conflict_tracking = 'column'.",
		},
	},
	{
		ID: "conflicts_delete", Title: "Delete-Konflikte", Trigger: "conflict_delete",
		Problem: "Eine Zeile wurde auf einer Seite gelöscht und auf der anderen geändert.",
		Causes: []string{
			"Physische DELETEs statt Soft-Deletes.",
			"Archivierungsjobs laufen auf beiden Seiten.",
		},
		Solutions: []string{
			"Soft-Deletes (IsDeleted-Flag) statt physischer DELETEs nutzen.",
			"Archivierungsjobs auf den Publisher zentralisieren.",
			"compensate_for_errors-Parameter des Merge Agents konfigurieren.",
		},
	},
	{
		ID: "blocking_repl", Title: "Replication Agent wird blockiert", Trigger: "blocking_repl_agent",
		Problem: "Der Merge-Agent (replmerg.exe) wird durch eine andere Session blockiert.",
		Causes: []string{
			"Offene Transaktionen durch Entwickler-Tools (SSMS) ohne COMMIT.",
			"ETL-Jobs oder Reporting-Queries mit Table-Locks.",
			"Fehlende Indizes → Lock-Escalation.",
		},
		Solutions: []string{
			"Blockierende Session identifizieren (host_name, login_name prüfen).",
			"READ_COMMITTED_SNAPSHOT aktivieren: ALTER DATABASE [DB] SET READ_COMMITTED_SNAPSHOT ON;",
			"Besitzer der blockierenden Session kontaktieren.",
			"LOCK_TIMEOUT im Merge Agent Profile setzen.",
			"SSMS-Queries ohne COMMIT sind der #1 Killer – Kollegen ansprechen.",
		},
	},
	{
		ID: "deadlock_repl", Title: "Deadlocks bei Merge-Replikation", Trigger: "deadlock_detected",
		Problem: "Deadlocks zwischen Merge-Agent und Applikation.",
		Causes: []string{
			"Konkurrierende Zugriffsmuster: App schreibt A→B, Merge liest B→A.",
			"Fehlende Indizes → breite Lock-Ranges.",
		},
		Solutions: []string{
			"Trace-Flag 1222 aktivieren: DBCC TRACEON(1222, -1);",
			"Deadlock-Graph in SQL Server Logs analysieren.",
			"Indizes der replizierten Tabellen optimieren.",
			"RCSI auf der Datenbank erwägen.",
		},
	},
	{
		ID: "session_failed", Title: "Merge-Session fehlgeschlagen", Trigger: "session_error",
		Problem: "Merge-Sessions mit Status 'Failed'.",
		Causes: []string{
			"Netzwerkunterbrechung.",
			"Merge-Agent Timeout.",
			"Nicht replizierte Schema-Änderungen.",
			"Berechtigungsprobleme.",
		},
		Solutions: []string{
			"'Last Message' Spalte prüfen – enthält die genaue Fehlermeldung.",
			"SQL Server Agent Job-Verlauf prüfen.",
			"Merge-Agent manuell mit Verbose-Logging starten: replmerg.exe -OutputVerboseLevel 2.",
			"Bei Schema-Fehlern: sp_helpmergepublication prüfen.",
			"Agent-Account braucht db_owner auf Publisher, Subscriber und Distribution DB.",
		},
	},
	{
		ID: "no_sessions", Title: "Keine Sessions in den letzten 60 Minuten", Trigger: "no_recent_sessions",
		Problem: "Keine Merge-Sessions gefunden. Der Agent läuft möglicherweise nicht.",
		Causes: []string{
			"SQL Server Agent gestoppt.",
			"Merge-Agent-Job deaktiviert.",
			"Job-Schedule nicht korrekt.",
		},
		Solutions: []string{
			"SQL Server Agent-Dienst prüfen.",
			"SQL Server Agent → Jobs → Merge-Agent-Job suchen.",
			"Schedule prüfen: Agent sollte 'Continuous' oder in kurzen Intervallen laufen.",
			"Agent manuell starten als Test.",
		},
	},
}

// ───────────────────────────────────────────────────────────────
// Data Types
// ───────────────────────────────────────────────────────────────

type ServerConfig struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Instance string `json:"instance"`
	AuthMode string `json:"auth_mode"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	DistDB   string `json:"distribution_db"`
	PubDB    string `json:"publication_db"`
}

type Config struct {
	Servers        []ServerConfig `json:"servers"`
	DefaultServer  string         `json:"default_server"`
	ListenPort     int            `json:"listen_port"`
	RefreshSeconds int            `json:"refresh_seconds"`
}

type TopologyNode struct {
	ServerName string `json:"server_name"`
	Role       string `json:"role"`
	RoleLabel  string `json:"role_label"`
	Status     string `json:"status"`
	Weight     int    `json:"weight"`
}

type TopologyLink struct {
	From           string `json:"from"`
	To             string `json:"to"`
	Publication    string `json:"publication"`
	UploadStatus   string `json:"upload_status"`
	DownloadStatus string `json:"download_status"`
}

type Topology struct {
	Nodes []TopologyNode `json:"nodes"`
	Links []TopologyLink `json:"links"`
}

type PubSubCard struct {
	Key             string `json:"key"`
	Publication     string `json:"publication"`
	Subscriber      string `json:"subscriber"`
	Level           string `json:"level"`
	StatusText      string `json:"status_text"`
	StatusClass     string `json:"status_class"`
	MaxDuration     int    `json:"max_duration"`
	TotalErrors     int    `json:"total_errors"`
	ConflictCount   int    `json:"conflict_count"`
	TotalUploads    int    `json:"total_uploads"`
	TotalDownloads  int    `json:"total_downloads"`
	LastMessage     string `json:"last_message"`
	LastStatus      int    `json:"last_status"`
	UploadStatus    string `json:"upload_status"`
	DownloadStatus  string `json:"download_status"`
	IsBlocked       bool   `json:"is_blocked"`
	RootCause       string `json:"root_cause"`
	RootCauseType   string `json:"root_cause_type"`
}

type HealthStatus struct {
	Level   string   `json:"level"`
	Summary string   `json:"summary"`
	Details []string `json:"details"`
}

type RootCauseAnalysis struct {
	Type        string `json:"type"`
	Label       string `json:"label"`
	Icon        string `json:"icon"`
	Explanation string `json:"explanation"`
}

type DashboardData struct {
	Timestamp        string                 `json:"timestamp"`
	Health           HealthStatus           `json:"health"`
	Topology         Topology               `json:"topology"`
	Cards            []PubSubCard           `json:"cards"`
	Sessions         []map[string]any       `json:"sessions"`
	Conflicts        []map[string]any       `json:"conflicts"`
	Blocking         []map[string]any       `json:"blocking"`
	ConflictCount    int                    `json:"conflict_count"`
	SessionCount     int                    `json:"session_count"`
	BlockingCount    int                    `json:"blocking_count"`
	FailedSessions   int                    `json:"failed_sessions"`
	RootCause        *RootCauseAnalysis     `json:"root_cause"`
	ServerName       string                 `json:"server_name"`
	AvailableServers []string               `json:"available_servers"`
	CurrentServer    string                 `json:"current_server"`
	Troubleshooting  []TroubleshootingEntry `json:"troubleshooting"`
}

// ───────────────────────────────────────────────────────────────
// Global State
// ───────────────────────────────────────────────────────────────

var (
	globalConfig   Config
	configFilePath = "servers.json"
	configMu       sync.Mutex
)

type ServerState struct {
	mu       sync.Mutex
	db       *sql.DB
	config   ServerConfig
	topology *Topology
}

var (
	stateMu      sync.RWMutex
	serverStates = map[string]*ServerState{}
	activeServer string
)

// ───────────────────────────────────────────────────────────────
// Config I/O
// ───────────────────────────────────────────────────────────────

func getConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return configFilePath
	}
	return filepath.Join(filepath.Dir(exe), configFilePath)
}

func loadConfig() {
	path := getConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		globalConfig = Config{ListenPort: 9090, RefreshSeconds: 5, Servers: []ServerConfig{}}
		return
	}
	if err := json.Unmarshal(data, &globalConfig); err != nil {
		globalConfig = Config{ListenPort: 9090, RefreshSeconds: 5}
		return
	}
	if globalConfig.ListenPort == 0 {
		globalConfig.ListenPort = 9090
	}
	if globalConfig.RefreshSeconds == 0 {
		globalConfig.RefreshSeconds = 5
	}
}

func saveConfig() error {
	configMu.Lock()
	defer configMu.Unlock()
	data, err := json.MarshalIndent(globalConfig, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), data, 0644)
}

// ───────────────────────────────────────────────────────────────
// Connection Management
// ───────────────────────────────────────────────────────────────

func buildConnString(sc ServerConfig, dbName string) string {
	host := sc.Host
	if sc.Instance != "" {
		host = fmt.Sprintf("%s\\%s", host, sc.Instance)
	}
	port := sc.Port
	if port == 0 {
		port = 1433
	}
	if sc.AuthMode == "windows" {
		return fmt.Sprintf(
			"sqlserver://%s:%d?database=%s&encrypt=true&TrustServerCertificate=true&connection+timeout=10&app+name=ReplicationDash",
			host, port, dbName,
		)
	}
	return fmt.Sprintf(
		"sqlserver://%s:%s@%s:%d?database=%s&encrypt=true&TrustServerCertificate=true&connection+timeout=10&app+name=ReplicationDash",
		sc.User, sc.Password, host, port, dbName,
	)
}

func getOrCreateState(serverName string) (*ServerState, error) {
	stateMu.RLock()
	st, ok := serverStates[serverName]
	stateMu.RUnlock()
	if ok {
		return st, nil
	}

	var sc *ServerConfig
	for i := range globalConfig.Servers {
		if globalConfig.Servers[i].Name == serverName {
			sc = &globalConfig.Servers[i]
			break
		}
	}
	if sc == nil {
		return nil, fmt.Errorf("server not found: %s", serverName)
	}

	if strings.EqualFold(sc.AuthMode, "mock") {
		st = &ServerState{config: *sc}
		stateMu.Lock()
		serverStates[serverName] = st
		stateMu.Unlock()
		return st, nil
	}

	connStr := buildConnString(*sc, sc.DistDB)
	db, err := sql.Open("sqlserver", connStr)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot connect to %s: %v", serverName, err)
	}

	st = &ServerState{db: db, config: *sc}
	stateMu.Lock()
	serverStates[serverName] = st
	stateMu.Unlock()
	return st, nil
}

func currentState() (*ServerState, error) {
	stateMu.RLock()
	name := activeServer
	stateMu.RUnlock()

	if name == "" {
		if len(globalConfig.Servers) == 0 {
			return nil, fmt.Errorf("no_servers")
		}
		name = globalConfig.Servers[0].Name
		stateMu.Lock()
		activeServer = name
		stateMu.Unlock()
	}
	return getOrCreateState(name)
}

// ───────────────────────────────────────────────────────────────
// Query Execution
// ───────────────────────────────────────────────────────────────

func executeQuery(db *sql.DB, query string) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var results []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := make(map[string]any)
		for i, col := range cols {
			v := values[i]
			switch t := v.(type) {
			case []byte:
				row[col] = string(t)
			case time.Time:
				row[col] = t.Format("2006-01-02 15:04:05")
			default:
				row[col] = t
			}
		}
		results = append(results, row)
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, rows.Err()
}

// ───────────────────────────────────────────────────────────────
// Root Cause Analysis
// ───────────────────────────────────────────────────────────────

func analyzeRootCause(sessions []map[string]any, conflicts []map[string]any, blocking []map[string]any) *RootCauseAnalysis {
	if len(sessions) == 0 && len(conflicts) == 0 && len(blocking) == 0 {
		return nil
	}

	// Check blocking first – most actionable
	for _, b := range blocking {
		replRelated := toInt(b["replication_related"]) == 1
		if !replRelated {
			continue
		}
		blockerProg := toString(b["blocker_program"])
		blockerHost := toString(b["blocker_host"])
		blockerLogin := toString(b["blocker_login"])
		blockerStatus := toString(b["blocker_status"])

		// SSMS / developer tools without commit
		if strings.Contains(strings.ToLower(blockerProg), "management studio") ||
			strings.Contains(strings.ToLower(blockerProg), "azdata") ||
			strings.Contains(strings.ToLower(blockerProg), "azure data studio") {
			return &RootCauseAnalysis{
				Type:  "user",
				Label: "Nutzungsspezifisch",
				Icon:  "👤",
				Explanation: fmt.Sprintf(
					"Merge-Agent wird blockiert durch %s auf %s (%s). Status: %s. Vermutlich eine offene Transaktion ohne COMMIT.",
					blockerProg, blockerHost, blockerLogin, blockerStatus),
			}
		}

		// ETL or app blocking
		if strings.Contains(strings.ToLower(blockerProg), "etl") ||
			strings.Contains(strings.ToLower(blockerProg), "dtsx") ||
			strings.Contains(strings.ToLower(blockerProg), "ssis") {
			return &RootCauseAnalysis{
				Type:  "user",
				Label: "Nutzungsspezifisch",
				Icon:  "👤",
				Explanation: fmt.Sprintf(
					"ETL-Job (%s) auf %s blockiert den Merge-Agent. Job-Zeitfenster prüfen oder RCSI aktivieren.",
					blockerProg, blockerHost),
			}
		}

		// sleeping blocker = someone left a transaction open
		if strings.EqualFold(blockerStatus, "sleeping") {
			return &RootCauseAnalysis{
				Type:  "user",
				Label: "Nutzungsspezifisch",
				Icon:  "👤",
				Explanation: fmt.Sprintf(
					"Session %s auf %s (%s) ist im Status 'sleeping' und blockiert den Merge-Agent. Offene Transaktion ohne COMMIT.",
					blockerLogin, blockerHost, blockerProg),
			}
		}

		// Generic blocking – could be systemic
		waitMs := toInt(b["wait_time_ms"])
		if waitMs > 30000 {
			return &RootCauseAnalysis{
				Type:  "system",
				Label: "Systemisch",
				Icon:  "⚙️",
				Explanation: fmt.Sprintf(
					"Merge-Agent wartet seit %d ms. Blockiert durch %s auf %s. Möglicherweise Ressourcen-Engpass (I/O, CPU).",
					waitMs, blockerProg, blockerHost),
			}
		}
	}

	// Check conflicts
	if len(conflicts) > 5 {
		return &RootCauseAnalysis{
			Type:  "user",
			Label: "Nutzungsspezifisch",
			Icon:  "👤",
			Explanation: fmt.Sprintf(
				"%d Konflikte erkannt. Ursache: Fehlende Datenhoheit – mehrere Standorte ändern dieselben Datensätze. Partitionierung der Zuständigkeiten prüfen.",
				len(conflicts)),
		}
	}

	// Check session patterns
	failedCount := 0
	highLatencyCount := 0
	highVolumeHighLatency := false
	lowVolumehighLatency := false

	for _, s := range sessions {
		if toInt(s["run_status"]) == 6 {
			failedCount++
			msg := toString(s["last_message"])
			if strings.Contains(strings.ToLower(msg), "deadlock") {
				return &RootCauseAnalysis{
					Type:        "user",
					Label:       "Nutzungsspezifisch",
					Icon:        "👤",
					Explanation: "Deadlock während der Merge-Synchronisation. Konkurrierende Zugriffsmuster zwischen Applikation und Merge-Agent. Indizes und Zugriffspfade optimieren.",
				}
			}
			if strings.Contains(strings.ToLower(msg), "timeout") {
				return &RootCauseAnalysis{
					Type:        "system",
					Label:       "Systemisch",
					Icon:        "⚙️",
					Explanation: "Merge-Agent Timeout. Server möglicherweise überlastet oder Netzwerk-Latenz zu hoch.",
				}
			}
			if strings.Contains(strings.ToLower(msg), "connect") {
				return &RootCauseAnalysis{
					Type:        "system",
					Label:       "Systemisch",
					Icon:        "⚙️",
					Explanation: "Verbindungsfehler zum Subscriber. Netzwerk, DNS oder SQL Server-Dienst auf dem Subscriber prüfen.",
				}
			}
		}

		dur := toInt(s["duration_seconds"])
		if dur > 300 {
			highLatencyCount++
			totalRows := toInt(s["upload_inserts"]) + toInt(s["upload_updates"]) + toInt(s["download_inserts"]) + toInt(s["download_updates"])
			if totalRows > 10000 {
				highVolumeHighLatency = true
			} else if totalRows < 100 {
				lowVolumehighLatency = true
			}
		}
	}

	if highVolumeHighLatency {
		return &RootCauseAnalysis{
			Type:        "user",
			Label:       "Nutzungsspezifisch",
			Icon:        "👤",
			Explanation: "Hohe Latenz bei großem Datenvolumen. Vermutlich Massen-Operation zur falschen Zeit. Batch-Jobs in Wartungsfenster verlegen.",
		}
	}

	if lowVolumehighLatency {
		return &RootCauseAnalysis{
			Type:        "system",
			Label:       "Systemisch",
			Icon:        "⚙️",
			Explanation: "Hohe Latenz trotz weniger Daten. Deutet auf I/O-Engpass, Netzwerkprobleme oder Server-Überlastung hin.",
		}
	}

	if failedCount > 0 {
		return &RootCauseAnalysis{
			Type:        "system",
			Label:       "Systemisch",
			Icon:        "⚙️",
			Explanation: fmt.Sprintf("%d fehlgeschlagene Sessions. Fehlerdetails in der Session-Tabelle prüfen.", failedCount),
		}
	}

	return nil
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case int32:
		return int(t)
	default:
		return 0
	}
}

func availableServerNames() []string {
	var names []string
	for _, s := range globalConfig.Servers {
		names = append(names, s.Name)
	}
	return names
}

// ───────────────────────────────────────────────────────────────
// Build Cards with per-card blocking check and root cause
// ───────────────────────────────────────────────────────────────

func buildCards(sessions []map[string]any, conflicts []map[string]any, blocking []map[string]any, topo *Topology) []PubSubCard {
	// Group sessions by pub+sub
	type group struct {
		pub, sub    string
		sessions    []map[string]any
		lastStatus  int
		lastStart   string
		maxDur      int
		totalErrors int
		totalUp     int
		totalDown   int
		lastMsg     string
		uploadPhase bool
		downloadPhase bool
	}

	groups := map[string]*group{}

	// Pre-populate from topology
	if topo != nil {
		for _, link := range topo.Links {
			key := link.Publication + " → " + link.To
			if _, ok := groups[key]; !ok {
				groups[key] = &group{pub: link.Publication, sub: link.To}
			}
		}
	}

	// Fill with session data
	for _, s := range sessions {
		pub := toString(s["publication_name"])
		sub := toString(s["subscriber"])
		key := pub + " → " + sub
		g, ok := groups[key]
		if !ok {
			g = &group{pub: pub, sub: sub}
			groups[key] = g
		}
		g.sessions = append(g.sessions, s)
		g.totalErrors += toInt(s["error_count"])
		g.totalUp += toInt(s["upload_inserts"]) + toInt(s["upload_updates"]) + toInt(s["upload_deletes"])
		g.totalDown += toInt(s["download_inserts"]) + toInt(s["download_updates"]) + toInt(s["download_deletes"])

		dur := toInt(s["duration_seconds"])
		if dur > g.maxDur {
			g.maxDur = dur
		}

		startTime := toString(s["start_time"])
		if startTime > g.lastStart {
			g.lastStart = startTime
			g.lastStatus = toInt(s["run_status"])
			g.lastMsg = toString(s["last_message"])
		}

		msg := strings.ToLower(toString(s["last_message"]))
		if strings.Contains(msg, "upload") {
			g.uploadPhase = true
		}
		if strings.Contains(msg, "download") {
			g.downloadPhase = true
		}
	}

	// Count conflicts per origin
	conflictCounts := map[string]int{}
	for _, c := range conflicts {
		origin := toString(c["origin_datasource"])
		conflictCounts[origin]++
	}

	// Check which programs are blocked
	blockedPrograms := map[string]bool{}
	for _, b := range blocking {
		if toInt(b["replication_related"]) == 1 {
			prog := toString(b["blocked_program"])
			blockedPrograms[strings.ToLower(prog)] = true
		}
	}
	hasReplBlocking := len(blockedPrograms) > 0

	// Build cards
	var cards []PubSubCard
	for key, g := range groups {
		confCount := conflictCounts[g.sub]

		// Per-card level
		level := "green"
		if g.lastStatus == 6 || g.totalErrors > 0 {
			level = "red"
		} else if hasReplBlocking {
			// only red if this specific sub might be affected
			level = "red"
		} else if g.maxDur > 300 || g.lastStatus == 5 {
			level = "yellow"
		} else if confCount > 0 {
			level = "yellow"
		}

		// If no sessions at all for this card
		if len(g.sessions) == 0 {
			level = "yellow"
		}

		statusText, statusClass := cardStatus(g.lastStatus, g.totalErrors, g.maxDur, len(g.sessions) == 0)

		// Upload/Download status
		upStatus := "ok"
		downStatus := "ok"
		if g.lastStatus == 6 {
			if g.uploadPhase {
				upStatus = "error"
			}
			if g.downloadPhase {
				downStatus = "error"
			}
			if !g.uploadPhase && !g.downloadPhase {
				upStatus = "error"
				downStatus = "error"
			}
		} else if g.lastStatus == 3 {
			if g.uploadPhase {
				upStatus = "running"
			}
			if g.downloadPhase {
				downStatus = "running"
			}
		}

		cards = append(cards, PubSubCard{
			Key:            key,
			Publication:    g.pub,
			Subscriber:     g.sub,
			Level:          level,
			StatusText:     statusText,
			StatusClass:    statusClass,
			MaxDuration:    g.maxDur,
			TotalErrors:    g.totalErrors,
			ConflictCount:  confCount,
			TotalUploads:   g.totalUp,
			TotalDownloads: g.totalDown,
			LastMessage:    g.lastMsg,
			LastStatus:     g.lastStatus,
			UploadStatus:   upStatus,
			DownloadStatus: downStatus,
			IsBlocked:      hasReplBlocking,
		})
	}

	return cards
}

func cardStatus(lastStatus int, errors int, maxDur int, noSessions bool) (string, string) {
	if noSessions {
		return "KEINE DATEN", "s-warn"
	}
	if lastStatus == 6 {
		return "FAILED", "s-crit"
	}
	if errors > 0 {
		return "ERRORS", "s-crit"
	}
	if lastStatus == 5 {
		return "RETRY", "s-warn"
	}
	if maxDur > 300 {
		return "LANGSAM", "s-warn"
	}
	if lastStatus == 3 || lastStatus == 1 {
		return "LÄUFT", "s-running"
	}
	if lastStatus == 2 {
		return "OK", "s-ok"
	}
	return "IDLE", "s-ok"
}

// ───────────────────────────────────────────────────────────────
// Health Assessment
// ───────────────────────────────────────────────────────────────

func assessHealth(sessions []map[string]any, conflicts []map[string]any, blocking []map[string]any, failed int) HealthStatus {
	level := "green"
	var details []string

	if failed > 0 {
		level = "red"
		details = append(details, fmt.Sprintf("%d fehlgeschlagene Session(s) in den letzten 60 Minuten", failed))
	}

	replBlocked := 0
	for _, b := range blocking {
		if toInt(b["replication_related"]) == 1 {
			replBlocked++
		}
	}
	if replBlocked > 0 {
		level = "red"
		details = append(details, fmt.Sprintf("%d Blockade(n) betreffen Replikations-Agenten", replBlocked))
	}

	for _, s := range sessions {
		d := toInt(s["duration_seconds"])
		if d > 300 {
			if level == "green" {
				level = "yellow"
			}
			details = append(details, fmt.Sprintf("Hohe Latenz: %d Sek. für %v → %v", d, s["publication_name"], s["subscriber"]))
			break
		}
	}

	if len(conflicts) > 5 {
		if level == "green" {
			level = "yellow"
		}
		details = append(details, fmt.Sprintf("%d Konflikte erkannt", len(conflicts)))
	}

	if len(sessions) == 0 {
		if level == "green" {
			level = "yellow"
		}
		details = append(details, "Keine Merge-Sessions in den letzten 60 Minuten")
	}

	if len(details) == 0 {
		details = append(details, "Alle Merge-Sessions laufen normal. Keine Blockaden oder Konflikte.")
	}

	summaryMap := map[string]string{
		"green":  "OK – Replikation läuft normal",
		"yellow": "Warnung – Auffälligkeiten erkannt",
		"red":    "Kritisch – Sofortiger Handlungsbedarf",
	}
	return HealthStatus{Level: level, Summary: summaryMap[level], Details: details}
}

// ───────────────────────────────────────────────────────────────
// Real Data Fetcher
// ───────────────────────────────────────────────────────────────

func fetchRealData(st *ServerState) (*DashboardData, error) {
	sc := st.config
	distDB := st.db
	if distDB == nil {
		return nil, fmt.Errorf("no database connection for %s", sc.Name)
	}

	// Topology discovery (once)
	if st.topology == nil {
		topo, err := discoverTopology(distDB, sc)
		if err == nil {
			st.topology = topo
		}
	}

	sessions, err := executeQuery(distDB, queryLatencySessions)
	if err != nil {
		return nil, fmt.Errorf("sessions query: %w", err)
	}

	pubConnStr := buildConnString(sc, sc.PubDB)
	pubDB, err := sql.Open("sqlserver", pubConnStr)
	if err != nil {
		return nil, fmt.Errorf("publication DB: %w", err)
	}
	defer pubDB.Close()

	conflicts, err := executeQuery(pubDB, queryConflicts)
	if err != nil {
		conflicts = []map[string]any{}
	}

	masterConnStr := buildConnString(sc, "master")
	masterDB, err := sql.Open("sqlserver", masterConnStr)
	if err != nil {
		return nil, fmt.Errorf("master DB: %w", err)
	}
	defer masterDB.Close()

	blocking, err := executeQuery(masterDB, queryBlocking)
	if err != nil {
		blocking = []map[string]any{}
	}

	failedCount := 0
	for _, s := range sessions {
		if toInt(s["run_status"]) == 6 {
			failedCount++
		}
	}

	health := assessHealth(sessions, conflicts, blocking, failedCount)
	rootCause := analyzeRootCause(sessions, conflicts, blocking)
	cards := buildCards(sessions, conflicts, blocking, st.topology)

	topo := st.topology
	if topo == nil {
		topo = &Topology{}
	}

	return &DashboardData{
		Timestamp:        time.Now().Format("2006-01-02 15:04:05"),
		Health:           health,
		Topology:         *topo,
		Cards:            cards,
		Sessions:         sessions,
		Conflicts:        conflicts,
		Blocking:         blocking,
		ConflictCount:    len(conflicts),
		SessionCount:     len(sessions),
		BlockingCount:    len(blocking),
		FailedSessions:   failedCount,
		RootCause:        rootCause,
		ServerName:       sc.Name,
		AvailableServers: availableServerNames(),
		CurrentServer:    sc.Name,
		Troubleshooting:  troubleshootingData,
	}, nil
}

func discoverTopology(db *sql.DB, sc ServerConfig) (*Topology, error) {
	rows, err := executeQuery(db, queryTopology)
	if err != nil {
		return nil, err
	}

	serverName := ""
	nodesMap := map[string]TopologyNode{}
	var links []TopologyLink

	for _, r := range rows {
		sn := toString(r["server_name"])
		if serverName == "" {
			serverName = sn
		}
		pub := toString(r["publication_name"])
		sub := toString(r["subscriber_server"])

		// Publisher node
		if _, ok := nodesMap[sn]; !ok {
			nodesMap[sn] = TopologyNode{
				ServerName: sn,
				Role:       "publisher_distributor",
				RoleLabel:  "Verleger & Verteiler",
				Weight:     1,
			}
		}

		// Subscriber node
		if sub != "" {
			if _, ok := nodesMap[sub]; !ok {
				nodesMap[sub] = TopologyNode{
					ServerName: sub,
					Role:       "subscriber",
					RoleLabel:  "Abonnent",
					Weight:     3,
				}
			}
			links = append(links, TopologyLink{
				From:        sn,
				To:          sub,
				Publication: pub,
			})
		}
	}

	var nodes []TopologyNode
	for _, n := range nodesMap {
		nodes = append(nodes, n)
	}

	return &Topology{Nodes: nodes, Links: links}, nil
}

// ───────────────────────────────────────────────────────────────
// Mock Data – Fixed Topology
// ───────────────────────────────────────────────────────────────

func randInt(max int) int {
	if max <= 0 {
		return 0
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(max)))
	return int(n.Int64())
}

func randChoice(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[randInt(len(items))]
}

// Fixed mock topology – always the same structure
var mockTopology = Topology{
	Nodes: []TopologyNode{
		{ServerName: "SRV-SQL-PUB", Role: "publisher_distributor", RoleLabel: "Verleger & Verteiler", Weight: 1},
		{ServerName: "SRV-SQL-SUB1", Role: "subscriber", RoleLabel: "Abonnent", Weight: 3},
		{ServerName: "SRV-SQL-SUB2", Role: "subscriber", RoleLabel: "Abonnent", Weight: 3},
	},
	Links: []TopologyLink{
		{From: "SRV-SQL-PUB", To: "SRV-SQL-SUB1", Publication: "PUB_Orders"},
		{From: "SRV-SQL-PUB", To: "SRV-SQL-SUB1", Publication: "PUB_Customers"},
		{From: "SRV-SQL-PUB", To: "SRV-SQL-SUB2", Publication: "PUB_Orders"},
		{From: "SRV-SQL-PUB", To: "SRV-SQL-SUB2", Publication: "PUB_Inventory"},
	},
}

type mockPubSub struct {
	pub string
	sub string
}

var mockPairs = []mockPubSub{
	{pub: "PUB_Orders", sub: "SRV-SQL-SUB1"},
	{pub: "PUB_Customers", sub: "SRV-SQL-SUB1"},
	{pub: "PUB_Orders", sub: "SRV-SQL-SUB2"},
	{pub: "PUB_Inventory", sub: "SRV-SQL-SUB2"},
}

func generateMockData(serverName string) *DashboardData {
	now := time.Now()
	statuses := []int{2, 2, 2, 3, 5, 6} // weighted: mostly OK
	statusTexts := map[int]string{2: "Succeeded", 3: "InProgress", 5: "Retry", 6: "Failed"}
	programs := []string{"replmerg.exe", "Microsoft SQL Server Management Studio", "azdata", "ETL_DailyLoad.dtsx", "MyApp.exe"}
	waitTypes := []string{"LCK_M_X", "LCK_M_S", "LCK_M_U", "LCK_M_IX", "PAGEIOLATCH_SH"}
	logins := []string{"DOMAIN\\svc_replication", "DOMAIN\\dev_mueller", "DOMAIN\\dev_schmidt", "sa", "DOMAIN\\etl_service"}
	hosts := []string{"SRV-SQL-PUB", "WS-DEV-MUELLER", "WS-DEV-SCHMIDT", "SRV-ETL-01"}

	// Generate sessions for each fixed pub/sub pair
	sessions := make([]map[string]any, 0)
	failedCount := 0

	for _, pair := range mockPairs {
		numSessions := 1 + randInt(3)
		for i := 0; i < numSessions; i++ {
			st := statuses[randInt(len(statuses))]
			dur := 10 + randInt(500)
			if st == 6 {
				failedCount++
			}
			errCount := 0
			if st == 6 {
				errCount = 1 + randInt(3)
			} else if st == 5 {
				errCount = 1
			}
			startT := now.Add(-time.Duration(randInt(55)+1) * time.Minute)

			sessions = append(sessions, map[string]any{
				"publication_name": pair.pub,
				"subscriber":       pair.sub,
				"subscriber_db":    "ReplicaDB",
				"session_id":       1000 + len(sessions),
				"start_time":       startT.Format("2006-01-02 15:04:05"),
				"end_time":         startT.Add(time.Duration(dur) * time.Second).Format("2006-01-02 15:04:05"),
				"duration_seconds": dur,
				"delivery_rate":    float64(50+randInt(950)) / 10.0,
				"upload_inserts":   randInt(200),
				"upload_updates":   randInt(500),
				"upload_deletes":   randInt(50),
				"download_inserts": randInt(300),
				"download_updates": randInt(600),
				"download_deletes": randInt(30),
				"error_count":      errCount,
				"run_status":       st,
				"run_status_text":  statusTexts[st],
				"last_message":     mockMessage(st),
			})
		}
	}

	// Conflicts – fixed origins
	conflictTypes := []string{"Update", "Delete", "Insert-Unique", "Upload-Delete"}
	reasons := []string{"Publisher wins based on priority.", "Subscriber change was rejected.", "Unique key violation during merge upload."}
	conflicts := make([]map[string]any, 0)
	for i := 0; i < randInt(6); i++ {
		conflicts = append(conflicts, map[string]any{
			"conflict_table":     "MSmerge_conflict_PUB_" + randChoice([]string{"Orders", "Customers", "Inventory"}),
			"origin_datasource":  randChoice([]string{"SRV-SQL-SUB1", "SRV-SQL-SUB2"}),
			"conflict_type_text": randChoice(conflictTypes),
			"reason_text":        randChoice(reasons),
			"create_time":        now.Add(-time.Duration(randInt(55)+1) * time.Minute).Format("2006-01-02 15:04:05"),
		})
	}

	// Blocking – sometimes none, sometimes with repl involvement
	blocking := make([]map[string]any, 0)
	if randInt(3) == 0 { // 33% chance of blocking
		blockedProg := "replmerg.exe"
		blockerProg := randChoice(programs[1:])
		blocking = append(blocking, map[string]any{
			"blocked_spid":        60 + randInt(40),
			"blocking_spid":       50 + randInt(10),
			"wait_type":           randChoice(waitTypes),
			"wait_time_ms":        1000 + randInt(120000),
			"wait_resource":       fmt.Sprintf("KEY: 7:72057594042%05d", randInt(99999)),
			"database_name":       "ReplicaDB",
			"blocked_sql_text":    mockBlockedSQL(),
			"blocked_program":     blockedProg,
			"blocked_host":        "SRV-SQL-PUB",
			"blocked_login":       randChoice(logins),
			"blocker_program":     blockerProg,
			"blocker_host":        randChoice(hosts),
			"blocker_login":       randChoice(logins),
			"blocker_status":      randChoice([]string{"sleeping", "running", "suspended"}),
			"blocker_sql_text":    mockBlockerSQL(),
			"replication_related": 1,
		})
	}

	health := assessHealth(sessions, conflicts, blocking, failedCount)
	rootCause := analyzeRootCause(sessions, conflicts, blocking)
	cards := buildCards(sessions, conflicts, blocking, &mockTopology)

	return &DashboardData{
		Timestamp:        now.Format("2006-01-02 15:04:05"),
		Health:           health,
		Topology:         mockTopology,
		Cards:            cards,
		Sessions:         sessions,
		Conflicts:        conflicts,
		Blocking:         blocking,
		ConflictCount:    len(conflicts),
		SessionCount:     len(sessions),
		BlockingCount:    len(blocking),
		FailedSessions:   failedCount,
		RootCause:        rootCause,
		ServerName:       serverName,
		AvailableServers: availableServerNames(),
		CurrentServer:    serverName,
		Troubleshooting:  troubleshootingData,
	}
}

func mockMessage(status int) string {
	msgs := map[int][]string{
		2: {"The merge completed successfully.", "Delivered 342 changes in 45 seconds."},
		3: {"Uploading changes to the Publisher...", "Downloading changes from the Publisher..."},
		5: {"Retrying after transient error. Attempt 2 of 5."},
		6: {"The process could not connect to subscriber 'SRV-SQL-SUB1'.", "Deadlock detected during merge upload phase.", "Timeout expired waiting for lock."},
	}
	return randChoice(msgs[status])
}

func mockBlockedSQL() string {
	return randChoice([]string{
		"exec sp_MSmerge_getrows @tablenick = 42, @rowguid = 'A1B2C3D4-...'",
		"UPDATE [dbo].[Orders] SET [Status] = 3 WHERE [rowguid] = '...'",
		"INSERT INTO [MSmerge_tombstone] ([rowguid], [tablenick]) VALUES (...)",
	})
}

func mockBlockerSQL() string {
	return randChoice([]string{
		"SELECT * FROM [dbo].[Orders] WITH (HOLDLOCK) WHERE CustomerID = 12345",
		"BEGIN TRANSACTION\nUPDATE [dbo].[Customers] SET Region = 'West' WHERE ID IN (SELECT ...)",
		"-- SSMS Query, kein COMMIT\nUPDATE [dbo].[Inventory] SET Qty = Qty - 1 WHERE ...",
	})
}

// ───────────────────────────────────────────────────────────────
// HTTP Handlers
// ───────────────────────────────────────────────────────────────

func jsonResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(data)
}

func errResponse(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func handleAllData(w http.ResponseWriter, r *http.Request) {
	st, err := currentState()
	if err != nil {
		if err.Error() == "no_servers" {
			jsonResponse(w, map[string]any{
				"no_servers":        true,
				"available_servers": []string{},
				"current_server":    "",
			})
			return
		}
		errResponse(w, 500, err.Error())
		return
	}

	var data *DashboardData
	if strings.EqualFold(st.config.AuthMode, "mock") {
		data = generateMockData(st.config.Name)
	} else {
		data, err = fetchRealData(st)
		if err != nil {
			errResponse(w, 500, err.Error())
			return
		}
	}
	jsonResponse(w, data)
}

func handleAddServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errResponse(w, 405, "method not allowed")
		return
	}
	var sc ServerConfig
	if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
		errResponse(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if sc.Name == "" {
		sc.Name = sc.Host
	}
	if sc.Port == 0 {
		sc.Port = 1433
	}
	if sc.DistDB == "" {
		sc.DistDB = "distribution"
	}
	if sc.PubDB == "" {
		sc.PubDB = "master"
	}

	if !strings.EqualFold(sc.AuthMode, "mock") {
		connStr := buildConnString(sc, sc.DistDB)
		db, err := sql.Open("sqlserver", connStr)
		if err != nil {
			errResponse(w, 500, "driver error: "+err.Error())
			return
		}
		db.SetConnMaxLifetime(10 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			errResponse(w, 500, "connection failed: "+err.Error())
			return
		}
		db.Close()
	}

	found := false
	for i, s := range globalConfig.Servers {
		if s.Name == sc.Name {
			globalConfig.Servers[i] = sc
			found = true
			break
		}
	}
	if !found {
		globalConfig.Servers = append(globalConfig.Servers, sc)
	}

	stateMu.Lock()
	if st, ok := serverStates[sc.Name]; ok {
		if st.db != nil {
			st.db.Close()
		}
		delete(serverStates, sc.Name)
	}
	activeServer = sc.Name
	stateMu.Unlock()

	saveConfig()
	jsonResponse(w, map[string]string{"status": "ok", "server": sc.Name})
}

func handleRemoveServer(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		errResponse(w, 400, "missing name")
		return
	}
	var newList []ServerConfig
	for _, s := range globalConfig.Servers {
		if s.Name != name {
			newList = append(newList, s)
		}
	}
	globalConfig.Servers = newList

	stateMu.Lock()
	if st, ok := serverStates[name]; ok {
		if st.db != nil {
			st.db.Close()
		}
		delete(serverStates, name)
	}
	if activeServer == name {
		if len(globalConfig.Servers) > 0 {
			activeServer = globalConfig.Servers[0].Name
		} else {
			activeServer = ""
		}
	}
	stateMu.Unlock()

	saveConfig()
	jsonResponse(w, map[string]string{"status": "ok"})
}

func handleSwitchServer(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		errResponse(w, 400, "missing name")
		return
	}
	if _, err := getOrCreateState(name); err != nil {
		errResponse(w, 500, err.Error())
		return
	}
	stateMu.Lock()
	activeServer = name
	stateMu.Unlock()
	jsonResponse(w, map[string]string{"status": "ok", "server": name})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, map[string]any{
		"refresh_seconds":   globalConfig.RefreshSeconds,
		"available_servers": availableServerNames(),
		"current_server":    activeServer,
	})
}

func handleTroubleshooting(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, troubleshootingData)
}

// ───────────────────────────────────────────────────────────────
// Main
// ───────────────────────────────────────────────────────────────

func main() {
	loadConfig()

	if len(globalConfig.Servers) == 0 {
		globalConfig.Servers = []ServerConfig{
			{
				Name:     "Demo (Mock)",
				Host:     "localhost",
				Port:     1433,
				AuthMode: "mock",
				DistDB:   "distribution",
				PubDB:    "AdventureWorks",
			},
		}
		saveConfig()
	}

	if len(globalConfig.Servers) > 0 {
		name := globalConfig.DefaultServer
		if name == "" {
			name = globalConfig.Servers[0].Name
		}
		activeServer = name
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/all", handleAllData)
	mux.HandleFunc("/api/switch", handleSwitchServer)
	mux.HandleFunc("/api/server/add", handleAddServer)
	mux.HandleFunc("/api/server/remove", handleRemoveServer)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/troubleshooting", handleTroubleshooting)

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	addr := fmt.Sprintf(":%d", globalConfig.ListenPort)
	log.Printf("ReplicationDash v0.2.0 → http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
