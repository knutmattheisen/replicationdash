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

// ---------------------------------------------------------------------------
// Embedded frontend assets
// ---------------------------------------------------------------------------

//go:embed static/*
var staticFiles embed.FS

// ---------------------------------------------------------------------------
// SQL query constants
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Troubleshooting dictionary (embedded)
// ---------------------------------------------------------------------------

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
		Problem: "Die Merge-Session dauert deutlich länger als erwartet (> 5 Minuten). Der Subscriber hinkt hinterher.",
		Causes: []string{
			"Große Transaktionsmengen durch Batch-Jobs",
			"Netzwerk-Engpässe zwischen Publisher und Subscriber",
			"Fehlende oder fragmentierte Indizes auf replizierten Tabellen",
			"Hohe Server-Auslastung (CPU / I/O)",
		},
		Solutions: []string{
			"Prüfe die Upload-/Download-Counts – wenn auffällig hoch, suche nach Massen-Operationen in der Applikation.",
			"Überprüfe den Netzwerkdurchsatz zwischen Publisher und Subscriber mit iperf3.",
			"Führe einen Index-Maintenance-Plan aus: ALTER INDEX ALL ON <table> REBUILD;",
			"Prüfe sys.dm_os_wait_stats nach den Top-Waits auf dem Server.",
			"Erwäge eine Änderung des Merge-Agent-Profils auf 'High Volume Server-to-Server'.",
		},
	},
	{
		ID: "conflicts_update", Title: "Update-Konflikte", Trigger: "conflict_update",
		Problem: "Dieselbe Zeile wurde auf Publisher und Subscriber gleichzeitig geändert. Der Standard-Resolver hat eine Seite verworfen.",
		Causes: []string{
			"Beide Seiten ändern dieselbe Geschäftslogik-Tabelle ohne klare Datenhoheit.",
			"Fehlende Partitionierung der Datenverantwortung (wer ändert was).",
		},
		Solutions: []string{
			"Implementiere eine klare Datenhoheit: Subscriber-Standorte ändern nur 'ihre' Datensätze.",
			"Nutze einen Custom Conflict Resolver (z.B. Prioritäts-basiert oder Zeitstempel-basiert).",
			"Prüfe, ob die Applikation RBAR (Row By Agonizing Row) statt Batch-Updates sendet – das multipliziert Konflikte.",
			"Aktiviere Column-Level Tracking: sp_changemergearticle @conflict_tracking = 'column'.",
		},
	},
	{
		ID: "conflicts_delete", Title: "Delete-Konflikte", Trigger: "conflict_delete",
		Problem: "Eine Zeile wurde auf einer Seite gelöscht und auf der anderen geändert.",
		Causes: []string{
			"Soft-Deletes werden nicht verwendet – physische DELETEs kollidieren mit Updates.",
			"Archivierungsjobs laufen auf beiden Seiten.",
		},
		Solutions: []string{
			"Nutze Soft-Deletes (IsDeleted-Flag) statt physischer DELETEs.",
			"Zentralisiere Archivierungs- und Bereinigungsjobs auf dem Publisher.",
			"Konfiguriere den compensate_for_errors-Parameter des Merge Agents.",
		},
	},
	{
		ID: "blocking_repl", Title: "Replication Agent wird blockiert", Trigger: "blocking_repl_agent",
		Problem: "Der Merge-Agent (replmerg.exe) wird durch eine andere Session blockiert und kann keine Änderungen synchronisieren.",
		Causes: []string{
			"Lang laufende Transaktionen durch Entwickler-Tools (SSMS, Azure Data Studio) ohne COMMIT.",
			"ETL-Jobs oder Reporting-Queries mit Table-Locks.",
			"Fehlende Indizes führen zu Escalation auf Table-Locks.",
		},
		Solutions: []string{
			"Identifiziere die blockierende Session über die Blocking-Chain-Tabelle und prüfe den SQL-Text.",
			"Setze READ_COMMITTED_SNAPSHOT: ALTER DATABASE [DB] SET READ_COMMITTED_SNAPSHOT ON;",
			"Kontaktiere den Besitzer der blockierenden Session – prüfe host_name und login_name.",
			"Nutze LOCK_TIMEOUT für Merge-Agent-Connections: Merge Agent Profile → QueryTimeout.",
			"Wenn es ein Entwickler-Tool ist: Sprich mit dem Kollegen. SSMS-Queries ohne COMMIT sind der #1 Killer.",
		},
	},
	{
		ID: "deadlock_repl", Title: "Deadlocks bei der Merge-Replikation", Trigger: "deadlock_detected",
		Problem: "Deadlocks treten auf, wenn Merge-Agent und Applikation gleichzeitig auf dieselben Ressourcen zugreifen.",
		Causes: []string{
			"Konkurrierende Zugriffsmuster: App schreibt A→B, Merge-Agent liest B→A.",
			"Fehlende Indizes führen zu breiten Lock-Ranges.",
		},
		Solutions: []string{
			"Aktiviere Trace-Flag 1222 für Deadlock-Logging: DBCC TRACEON(1222, -1);",
			"Analysiere den Deadlock-Graph in den SQL Server Logs oder Extended Events.",
			"Optimiere die Indexierung der replizierten Tabellen.",
			"Erwäge RCSI (Read Committed Snapshot Isolation) auf der Datenbank.",
			"Prüfe, ob der Merge-Agent in der Upload- oder Download-Phase hängt.",
		},
	},
	{
		ID: "session_failed", Title: "Merge-Session fehlgeschlagen", Trigger: "session_error",
		Problem: "Eine oder mehrere Merge-Sessions sind mit Status 'Failed' beendet.",
		Causes: []string{
			"Netzwerkunterbrechung während der Synchronisation.",
			"Timeout des Merge-Agents.",
			"Schema-Änderungen, die nicht korrekt repliziert wurden.",
			"Berechtigungsprobleme auf Publisher oder Subscriber.",
		},
		Solutions: []string{
			"Prüfe die 'Last Message' Spalte – sie enthält die genaue Fehlermeldung.",
			"Prüfe den SQL Server Agent Job-Verlauf für den Merge-Agent-Job.",
			"Starte den Merge-Agent manuell mit Verbose-Logging: replmerg.exe -OutputVerboseLevel 2.",
			"Bei Schema-Fehlern: Prüfe sp_helpmergepublication und sp_helpmergearticle.",
			"Bei Berechtigungsfehlern: Der Agent-Account braucht db_owner auf Publisher, Subscriber und Distribution DB.",
		},
	},
	{
		ID: "no_sessions", Title: "Keine Merge-Sessions in den letzten 60 Minuten", Trigger: "no_recent_sessions",
		Problem: "Es wurden keine Merge-Sessions gefunden. Der Agent läuft möglicherweise nicht.",
		Causes: []string{
			"SQL Server Agent ist gestoppt.",
			"Der Merge-Agent-Job ist deaktiviert.",
			"Der Job-Schedule ist nicht korrekt konfiguriert.",
		},
		Solutions: []string{
			"Prüfe den Status des SQL Server Agent-Dienstes.",
			"Öffne SQL Server Agent → Jobs → suche nach dem Merge-Agent-Job.",
			"Prüfe den Job-Schedule: Der Agent sollte 'Continuous' oder in kurzen Intervallen laufen.",
			"Starte den Agent manuell als Test: Rechtsklick → Start Job at Step...",
		},
	},
}

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

type ServerConfig struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Instance string `json:"instance"`
	AuthMode string `json:"auth_mode"` // "windows", "sql", "mock"
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

type HealthStatus struct {
	Level   string   `json:"level"`
	Summary string   `json:"summary"`
	Details []string `json:"details"`
}

type DashboardData struct {
	Timestamp       string                 `json:"timestamp"`
	Health          HealthStatus           `json:"health"`
	Sessions        []map[string]any       `json:"sessions"`
	Conflicts       []map[string]any       `json:"conflicts"`
	Blocking        []map[string]any       `json:"blocking"`
	ConflictCount   int                    `json:"conflict_count"`
	SessionCount    int                    `json:"session_count"`
	BlockingCount   int                    `json:"blocking_count"`
	FailedSessions  int                    `json:"failed_sessions"`
	ServerName      string                 `json:"server_name"`
	AvailableServers []string              `json:"available_servers"`
	CurrentServer   string                 `json:"current_server"`
	Troubleshooting []TroubleshootingEntry `json:"troubleshooting"`
}

// ---------------------------------------------------------------------------
// Global state (same pattern as WaitDash)
// ---------------------------------------------------------------------------

var (
	globalConfig   Config
	configFilePath = "servers.json"
	configMu       sync.Mutex
)

type ServerState struct {
	mu     sync.Mutex
	db     *sql.DB
	config ServerConfig
}

var (
	stateMu      sync.RWMutex
	serverStates = map[string]*ServerState{}
	activeServer string
)

// ---------------------------------------------------------------------------
// Config I/O
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Connection management
// ---------------------------------------------------------------------------

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
			"sqlserver://%s:%d?database=%s&encrypt=true&TrustServerCertificate=true&connection+timeout=10&app+name=MergeDash",
			host, port, dbName,
		)
	}
	return fmt.Sprintf(
		"sqlserver://%s:%s@%s:%d?database=%s&encrypt=true&TrustServerCertificate=true&connection+timeout=10&app+name=MergeDash",
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

	// Mock mode: no real connection
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

// ---------------------------------------------------------------------------
// Query execution helper
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Real data fetcher
// ---------------------------------------------------------------------------

func fetchRealData(st *ServerState) (*DashboardData, error) {
	sc := st.config

	// Distribution DB connection
	distDB := st.db
	if distDB == nil {
		return nil, fmt.Errorf("no database connection for %s", sc.Name)
	}

	// Sessions from distribution DB
	sessions, err := executeQuery(distDB, queryLatencySessions)
	if err != nil {
		return nil, fmt.Errorf("sessions query: %w", err)
	}

	// Publication DB connection for conflicts
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

	// Blocking from master
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
		if v := toInt(s["run_status"]); v == 6 {
			failedCount++
		}
	}

	health := assessHealth(sessions, conflicts, blocking, failedCount)

	servers := availableServerNames()
	return &DashboardData{
		Timestamp:        time.Now().Format("2006-01-02 15:04:05"),
		Health:           health,
		Sessions:         sessions,
		Conflicts:        conflicts,
		Blocking:         blocking,
		ConflictCount:    len(conflicts),
		SessionCount:     len(sessions),
		BlockingCount:    len(blocking),
		FailedSessions:   failedCount,
		ServerName:       sc.Name,
		AvailableServers: servers,
		CurrentServer:    sc.Name,
		Troubleshooting:  troubleshootingData,
	}, nil
}

// ---------------------------------------------------------------------------
// Health assessment
// ---------------------------------------------------------------------------

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
		details = append(details, fmt.Sprintf("%d Konflikte erkannt – Datenhoheits-Konzept prüfen", len(conflicts)))
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

// ---------------------------------------------------------------------------
// Mock data generator
// ---------------------------------------------------------------------------

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

func generateMockData(serverName string) *DashboardData {
	now := time.Now()
	pubs := []string{"PUB_Orders", "PUB_Customers", "PUB_Inventory"}
	subs := []string{"SUB-BERLIN", "SUB-MUENCHEN", "SUB-HAMBURG"}
	statuses := []int{2, 3, 5, 6}
	statusTexts := map[int]string{2: "Succeeded", 3: "InProgress", 5: "Retry", 6: "Failed"}
	programs := []string{"replmerg.exe", "Microsoft SQL Server Management Studio", "azdata", "ETL_DailyLoad.dtsx", "MyApp.exe"}
	waitTypes := []string{"LCK_M_X", "LCK_M_S", "LCK_M_U", "LCK_M_IX", "PAGEIOLATCH_SH"}
	logins := []string{"DOMAIN\\svc_replication", "DOMAIN\\dev_mueller", "DOMAIN\\dev_schmidt", "sa", "DOMAIN\\etl_service"}
	hosts := []string{"SRV-SQL-01", "WS-DEV-MUELLER", "WS-DEV-SCHMIDT", "SRV-ETL-01"}

	sessions := make([]map[string]any, 0)
	failedCount := 0
	for i := 0; i < 6+randInt(5); i++ {
		st := statuses[randInt(len(statuses))]
		dur := 10 + randInt(600)
		if st == 6 {
			failedCount++
		}
		startT := now.Add(-time.Duration(randInt(55)+1) * time.Minute)
		errCount := 0
		if st == 6 {
			errCount = 1 + randInt(5)
		} else if st == 5 {
			errCount = 1
		}
		sessions = append(sessions, map[string]any{
			"publication_name": randChoice(pubs),
			"subscriber":       randChoice(subs),
			"subscriber_db":    "ReplicaDB",
			"session_id":       1000 + i,
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

	conflictTypes := []string{"Update", "Delete", "Insert-Unique", "Upload-Delete"}
	reasons := []string{
		"Publisher wins based on priority.",
		"Subscriber change was rejected.",
		"Unique key violation during merge upload.",
	}
	conflicts := make([]map[string]any, 0)
	for i := 0; i < 2+randInt(8); i++ {
		conflicts = append(conflicts, map[string]any{
			"conflict_table":     "MSmerge_conflict_PUB_" + randChoice([]string{"Orders", "Customers", "Inventory"}),
			"origin_datasource":  randChoice(subs),
			"conflict_type_text": randChoice(conflictTypes),
			"reason_text":        randChoice(reasons),
			"create_time":        now.Add(-time.Duration(randInt(55)+1) * time.Minute).Format("2006-01-02 15:04:05"),
		})
	}

	blocking := make([]map[string]any, 0)
	for i := 0; i < 1+randInt(3); i++ {
		isReplRelated := randInt(3) != 0
		blockedProg := "replmerg.exe"
		blockerProg := randChoice(programs[1:])
		if !isReplRelated {
			blockedProg = randChoice(programs)
			blockerProg = randChoice(programs)
		}
		replFlag := 0
		if isReplRelated {
			replFlag = 1
		}
		blocking = append(blocking, map[string]any{
			"blocked_spid":        60 + randInt(40),
			"blocking_spid":       50 + randInt(10),
			"wait_type":           randChoice(waitTypes),
			"wait_time_ms":        1000 + randInt(120000),
			"wait_resource":       fmt.Sprintf("KEY: 7:72057594042%05d", randInt(99999)),
			"database_name":       "ReplicaDB",
			"blocked_sql_text":    mockBlockedSQL(),
			"blocked_program":     blockedProg,
			"blocked_host":        "SRV-SQL-01",
			"blocked_login":       randChoice(logins),
			"blocker_program":     blockerProg,
			"blocker_host":        randChoice(hosts),
			"blocker_login":       randChoice(logins),
			"blocker_status":      randChoice([]string{"sleeping", "running", "suspended"}),
			"blocker_sql_text":    mockBlockerSQL(),
			"replication_related": replFlag,
		})
	}

	health := assessHealth(sessions, conflicts, blocking, failedCount)
	servers := availableServerNames()

	return &DashboardData{
		Timestamp:        now.Format("2006-01-02 15:04:05"),
		Health:           health,
		Sessions:         sessions,
		Conflicts:        conflicts,
		Blocking:         blocking,
		ConflictCount:    len(conflicts),
		SessionCount:     len(sessions),
		BlockingCount:    len(blocking),
		FailedSessions:   failedCount,
		ServerName:       serverName,
		AvailableServers: servers,
		CurrentServer:    serverName,
		Troubleshooting:  troubleshootingData,
	}
}

func mockMessage(status int) string {
	msgs := map[int][]string{
		2: {"The merge completed successfully.", "Delivered 342 changes in 45 seconds."},
		3: {"Uploading changes to the Publisher...", "Downloading changes from the Publisher..."},
		5: {"Retrying after transient error. Attempt 2 of 5."},
		6: {"The process could not connect to subscriber 'SUB-BERLIN'.", "Deadlock detected during merge upload phase.", "Timeout expired waiting for lock."},
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
		"INSERT INTO #TempImport SELECT * FROM OPENROWSET(...)",
		"-- SSMS Query, kein COMMIT\nUPDATE [dbo].[Inventory] SET Qty = Qty - 1 WHERE ...",
	})
}

// ---------------------------------------------------------------------------
// HTTP Handlers (same pattern as WaitDash)
// ---------------------------------------------------------------------------

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

	// Test connection (skip for mock)
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

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	loadConfig()

	// Create default mock server if no config
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

	// API endpoints (same pattern as WaitDash)
	mux.HandleFunc("/api/all", handleAllData)
	mux.HandleFunc("/api/switch", handleSwitchServer)
	mux.HandleFunc("/api/server/add", handleAddServer)
	mux.HandleFunc("/api/server/remove", handleRemoveServer)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/troubleshooting", handleTroubleshooting)

	// Serve embedded static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	addr := fmt.Sprintf(":%d", globalConfig.ListenPort)
	log.Printf("MergeDash v0.1 → http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
