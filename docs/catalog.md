# Table catalog

pgsink follows each Apiary table differently, because Apiary writes them
differently. The catalog lives in
[`internal/catalog/tables.yaml`](https://github.com/orlandoburli/apiary-pgsink/blob/main/internal/catalog/tables.yaml)
and is **data, not code**: a schema change is an edit there plus a release.

Run `pgsink doctor` to compare the catalog against a live database.

## Why classes exist

Apiary's most valuable reporting rows are written twice. `CreateExecution`
inserts a `task_executions` row at dispatch with `status='running'` and zero
cost; `UpdateExecution` fills in tokens, cost, timings and final status when the
agent finishes. A cursor on `id` alone would replicate the empty row and never
see the update — **every cost figure in PostgreSQL would be zero**. `step_runs`
has the same lifecycle and carries no `updated_at` at all.

## The five classes

| Class | Applies when | How pgsink follows it |
|---|---|---|
| `append_only` | Rows are never updated after insert | Monotonic cursor. Exact and cheap. |
| `mutable` | Rows are updated in place and carry `updated_at` | Cursor on `updated_at` with an overlap window; the idempotent upsert absorbs re-delivery. |
| `open_row` | Rows are updated in place with **no** `updated_at` | Cursor covers inserts; rows in a non-terminal state are re-read every cycle. That set is bounded by concurrency — a handful of rows, not a scan. |
| `follow_parent` | No usable cursor of its own | Re-read whenever the parent row moves. |
| `snapshot` | Small enough to compare wholesale | Full compare each cycle. |

## Drift

`pgsink doctor` reflects the live schema and reports differences.

- **Errors** mean replication would be incorrect — a cursor, key or state column
  pgsink depends on has changed.
- **Warnings** mean something moved that is worth a look but is still safe.

Columns Apiary *adds* are deliberately not reported. pgsink replicates whatever
reflection returns, so a new column flows through without a catalog edit — an
Apiary upgrade must never require a pgsink release.

## Keeping snapshots current

Compatibility is pinned to published Apiary artifacts, not to its source. The
test suite builds fixture databases from schema snapshots captured by booting a
real release binary:

```bash
scripts/capture-schema.sh /path/to/apiary 0.18
```

The whole catalog is then checked against every snapshot in `testdata/schema`,
so a drift shows up as a failing test that names the table and the column.
