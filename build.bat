@echo off
echo ═══════════════════════════════════════════
echo   MergeDash — Windows Build
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

go build -ldflags="-s -w -H windowsgui" -o mergedash.exe .
if %ERRORLEVEL% neq 0 (
    echo FEHLER: Build fehlgeschlagen
    pause
    exit /b 1
)

echo.
echo BUILD ERFOLGREICH: mergedash.exe
echo Starte mit: mergedash.exe
echo Oeffne: http://localhost:9090
echo.
pause
