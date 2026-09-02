#!/bin/bash
set -e
mkdir -p /home/ubuntu/new-api/snapshots
/usr/bin/sqlite3 /home/ubuntu/new-api/one-api.db ".backup /home/ubuntu/new-api/snapshots/pre-homev4-20260902.db"
echo "=== snapshot ==="
ls -la /home/ubuntu/new-api/snapshots/
echo "=== channels: id | name | status | models ==="
/usr/bin/sqlite3 /home/ubuntu/new-api/one-api.db "SELECT id || ' | ' || name || ' | status=' || status || ' | ' || models FROM channels;"
echo "=== HomePageContent current length ==="
/usr/bin/sqlite3 /home/ubuntu/new-api/one-api.db "SELECT length(value) FROM options WHERE key='HomePageContent';"
