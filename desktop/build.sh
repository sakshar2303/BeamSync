#!/usr/bin/env bash
# BeamSync release builder — produces Linux binary + Windows NSIS installer
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║     BeamSync V2 — Release Build      ║"
echo "╚══════════════════════════════════════╝"
echo ""

BIN_DIR="$SCRIPT_DIR/build/bin"
mkdir -p "$BIN_DIR"

# ── 1. Linux amd64 ────────────────────────────────────────────────────────────
echo "▶  [1/2] Building Linux amd64..."
wails build -platform linux/amd64 -clean
echo "   ✅  $BIN_DIR/BeamSync"

# ── 2. Windows amd64 (EXE + NSIS installer) ───────────────────────────────────
echo ""
echo "▶  [2/2] Building Windows amd64 (NSIS installer)..."
CC=x86_64-w64-mingw32-gcc CGO_ENABLED=1 wails build -platform windows/amd64 -nsis
echo "   ✅  $BIN_DIR/BeamSync.exe"
echo "   ✅  $BIN_DIR/BeamSync-amd64-installer.exe"

echo ""
echo "═══════════════════════════════════════"
echo "  BUILD COMPLETE"
echo "  Output → $(realpath "$BIN_DIR")"
echo "═══════════════════════════════════════"
ls -lh "$BIN_DIR"
echo ""
