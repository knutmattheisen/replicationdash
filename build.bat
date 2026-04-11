@echo off
echo ═══════════════════════════════════════════
echo   ReplicationDash v0.2.0 — Windows Build
echo ═══════════════════════════════════════════

go mod tidy
if %ERRORLEVEL% neq 0 (
    echo FEHLER: go mod tidy fehlgeschlagen
    pause
    exit /b 1
)

set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64

go build -ldflags="-s -w -H windowsgui" -o replicationdash.exe .
if %ERRORLEVEL% neq 0 (
    echo FEHLER: Build fehlgeschlagen
    pause
    exit /b 1
)

echo.
echo BUILD ERFOLGREICH: replicationdash.exe
echo Starte mit: replicationdash.exe
echo Oeffne: http://localhost:9090
echo.
pause
