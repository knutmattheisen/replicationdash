#!/bin/bash
echo "═══════════════════════════════════════════"
echo "  MergeDash — Cross-Compile (Linux → Windows)"
echo "═══════════════════════════════════════════"

go mod tidy || { echo "FEHLER: go mod tidy"; exit 1; }

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
    go build -ldflags="-s -w -H windowsgui" -o mergedash.exe .

if [ $? -eq 0 ]; then
    echo ""
    echo "BUILD ERFOLGREICH: mergedash.exe"
    ls -lh mergedash.exe
else
    echo "FEHLER: Build fehlgeschlagen"
    exit 1
fi
