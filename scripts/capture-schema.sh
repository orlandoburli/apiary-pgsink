#!/usr/bin/env bash
# Capture an Apiary schema snapshot for the test suite.
#
# Compatibility is pinned to published artifacts, not to Apiary's source: this
# boots a real apiary binary in a throwaway directory, lets it create and
# migrate its database, and dumps the resulting schema. No submodule, no
# vendoring, no source dependency between the two repositories.
#
#   scripts/capture-schema.sh <path-to-apiary-binary> <version-label>
#
# Example:
#   scripts/capture-schema.sh ./apiary 0.18
set -euo pipefail

BIN=${1:?usage: capture-schema.sh <apiary-binary> <version-label>}
LABEL=${2:?usage: capture-schema.sh <apiary-binary> <version-label>}
BIN=$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")
OUT="$(cd "$(dirname "$0")/.." && pwd)/testdata/schema/apiary-${LABEL}.sql"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

cd "$WORK"
"$BIN" init >/dev/null

# The daemon creates and migrates the database on startup. Give it a moment,
# then stop it so SQLite checkpoints the WAL back into the main file.
"$BIN" run >/dev/null 2>&1 &
DAEMON=$!
for _ in $(seq 1 50); do
  [ -f "$WORK/.apiary/apiary.db" ] && break
  perl -e 'select undef, undef, undef, 0.2'
done
perl -e 'select undef, undef, undef, 1.0'
kill "$DAEMON" 2>/dev/null || true
wait "$DAEMON" 2>/dev/null || true

if [ ! -f "$WORK/.apiary/apiary.db" ]; then
  echo "apiary did not create a database in $WORK/.apiary" >&2
  exit 1
fi

sqlite3 "$WORK/.apiary/apiary.db" ".schema" | grep -v '^CREATE TABLE sqlite_' > "$OUT"
echo "→ $OUT  ($(grep -c 'CREATE TABLE' "$OUT") tables)"
