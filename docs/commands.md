# Commands

## `pgsink doctor`

Checks the table catalog against a live Apiary database, and optionally a
configuration against both.

```bash
pgsink doctor --db ~/.apiary/apiary.db --config pgsink.yaml
```

Errors mean replication would be incorrect. Warnings mean something moved that
is worth a look but is still safe — a table absent from an older Apiary, for
instance, which is simply skipped.

## `pgsink migrate`

Creates or alters the target tables so they can hold what the configuration
selects.

```bash
pgsink migrate -c pgsink.yaml --dry-run   # print the DDL
pgsink migrate -c pgsink.yaml             # apply it
```

**Additive only.** A column whose type has changed, or one the target has that
the plan no longer wants, is reported rather than altered or dropped. Both are
destructive, and that is an operator's decision, not a sync loop's. Changes
apply in one transaction, so a failed migrate leaves the target as it was.

## `pgsink backfill`

Loads history.

```bash
pgsink backfill -c pgsink.yaml
pgsink backfill -c pgsink.yaml --tables step_runs,task_executions
pgsink backfill -c pgsink.yaml --migrate   # apply missing DDL first
```

Backfill is the same pipeline `sync` uses, with the watermark starting at zero
and a stop condition at the end of each table. **Re-running it is safe**: every
write is an idempotent upsert on the primary key.

Each table's cursor position is read *before* its scan and recorded afterwards,
so rows a running daemon writes during the backfill sit above the watermark and
are picked up by the first sync pass. Recording it afterwards would skip them.

## What the target looks like

One table per source table, in the configured schema, plus a watermark table.

Every replicated table carries an `apiary_instance` column, and it is the first
part of the primary key. This is structural rather than an extra field: Apiary's
ids are unique within one installation, but two installations mint colliding
autoincrement ids by construction, so a target that can hold more than one needs
the instance in the key.

```
apiary.apiary_sync_state
  apiary_instance, table_name, cursor_kind, cursor_value, rows_total, updated_at
```

The watermark lives in the target, so the sink itself is stateless — move it to
another host and it resumes from where the data actually is.

## Type mapping

| SQLite declares | PostgreSQL gets |
|---|---|
| `TEXT` | `text` |
| `INTEGER` | `bigint` |
| `REAL` | `double precision` |
| `BOOLEAN` | `boolean` |
| `TIMESTAMP`, `DATETIME` | `timestamptz` |
| a column named in `json_columns` | `jsonb` |

`BOOLEAN` and `TIMESTAMP` are split out ahead of SQLite's own affinity rules,
which fold both into NUMERIC — losing exactly the distinction worth keeping.

JSON is **declared, never sniffed**. Apiary stores JSON documents in TEXT
columns, but a TEXT column that merely looks like JSON stays `text`, so one
stray value cannot fail a whole batch.

Values are converted on the way through where the two type systems disagree:

- SQLite has no boolean storage class, so a `BOOLEAN` column holds 0 and 1.
- Apiary writes times as text, in several shapes across its history. Rows
  written before its `_time_format` fix carry a Go `time.Time.String()` suffix
  — a monotonic clock reading — that no standard parser accepts. The backfill
  meets those in the historical data and trims it rather than discarding the
  value. A timestamp that still cannot be parsed becomes NULL rather than
  failing the batch.
- Empty text is not valid JSON, so it lands as NULL.
