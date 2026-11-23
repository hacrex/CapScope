#!/bin/bash
# capscope-backup.sh — Backup and restore for CapScope SQLite database
#
# CapScope stores everything in one SQLite file (DB_PATH, default /data/capscope.db).
# This script backs up that file plus the session file, using SQLite's online
# backup API where possible so the backup is consistent while CapScope is running.
#
# Usage:
#   ./capscope-backup.sh backup   # create a timestamped backup
#   ./capscope-backup.sh restore <backup-file>   # restore from a backup
#   ./capscope-backup.sh verify   # verify the live DB is consistent

set -euo pipefail

DB_PATH="${CAPSCOPE_DB_PATH:-/data/capscope.db}"
SESSION_FILE="${DB_PATH}.sessions"
BACKUP_DIR="${CAPSCOPE_BACKUP_DIR:-/backups}"
TIMESTAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_FILE="${BACKUP_DIR}/capscope-${TIMESTAMP}.db"

mkdir -p "$BACKUP_DIR"

cmd="${1:-}"

case "$cmd" in
  backup)
    echo "[backup] Backing up $DB_PATH to $BACKUP_FILE"
    if command -v sqlite3 >/dev/null 2>&1; then
      # Online backup via sqlite3 .backup (consistent, no lock)
      sqlite3 "$DB_PATH" ".backup '$BACKUP_FILE'"
    else
      # Fallback: file copy (DB may be briefly inconsistent if a write is in flight;
      # SQLite WAL makes this safe in practice, but sqlite3 is preferred)
      cp "$DB_PATH" "$BACKUP_FILE"
    fi
    # Back up session file too, if it exists
    if [[ -f "$SESSION_FILE" ]]; then
      cp "$SESSION_FILE" "${BACKUP_FILE}.sessions"
    fi
    echo "[backup] Done: $BACKUP_FILE"
    ;;

  restore)
    src="${2:-}"
    if [[ -z "$src" ]] || [[ ! -f "$src" ]]; then
      echo "Usage: $0 restore <backup-file>"
      exit 1
    fi
    echo "[restore] Stopping CapScope is recommended before restoring."
    read -rp "Have you stopped CapScope? (yes/no): " confirm
    if [[ "$confirm" != "yes" ]]; then
      echo "[restore] Aborted."
      exit 1
    fi
    echo "[restore] Restoring $src to $DB_PATH"
    cp "$src" "$DB_PATH"
    if [[ -f "${src}.sessions" ]]; then
      cp "${src}.sessions" "$SESSION_FILE"
      chmod 600 "$SESSION_FILE"
    fi
    echo "[restore] Done. Start CapScope again."
    ;;

  verify)
    echo "[verify] Checking $DB_PATH integrity"
    if command -v sqlite3 >/dev/null 2>&1; then
      sqlite3 "$DB_PATH" "PRAGMA integrity_check;" | head -5
      sqlite3 "$DB_PATH" "PRAGMA foreign_key_check;" | head -5
    else
      echo "[verify] sqlite3 not found; skipping integrity check"
    fi
    ;;

  *)
    echo "Usage: $0 {backup|restore|verify}"
    exit 1
    ;;
esac