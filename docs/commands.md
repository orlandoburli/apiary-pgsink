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

## `pgsink sync`

Follows the source until interrupted.

```bash
pgsink sync -c pgsink.yaml            # follow
pgsink sync -c pgsink.yaml --once     # a single pass
pgsink sync -c pgsink.yaml -v         # report every pass, including empty ones
```

What "changed" means depends on how Apiary writes the table:

| Class | Selected each pass |
|---|---|
| `append_only` | rows past the watermark |
| `mutable` | rows at or after the watermark, stepped back by `sync.overlap` |
| `open_row` | rows past the watermark, **plus** every row the target still holds as unsettled |
| `follow_parent` | rows whose parent has moved |
| `snapshot` | the whole table, which is small |

### Why the open_row rescan reads the target

`task_executions` and `step_runs` are inserted at dispatch with
`status='running'` and zero cost, and updated at completion with the tokens,
cost and timings. Neither carries `updated_at`, and neither changes its key.

The rescan therefore asks **the target** what it still holds as unsettled, not
the source what is unsettled now. A row that completes between two passes is
already settled in the source and its cursor has not moved, so a source-side
question misses exactly the completion the class exists to capture. The target's
answer cannot: the row stays in that set until the pass that settles it.

The set is bounded by how much work Apiary can have in flight, so it is normally
a handful of rows.

!!! warning "Filtering on a state column changes when rows appear"

    A filter like `{column: status, op: in, value: [success, failed]}` on
    `task_executions` is a reasonable thing to want — only completed executions
    in your reporting. But it means a row is **invisible until it settles**, and
    then arrives complete. The open-row rescan has nothing to do, because the
    unsettled row was never replicated.

    That is a supported choice, not a bug. Just know which one you picked.

### Timestamp cursors are compared in UTC

Apiary writes timestamps with a local UTC offset. Compared as raw text,
`2026-08-07 02:00:00+00:00` sorts *after* `2026-08-06 23:00:00-04:00` — but the
second is an hour later. A watermark taken that way skips every row in between,
permanently. pgsink normalises through `strftime` before comparing or recording.

A row whose timestamp cannot be parsed at all is excluded from the incremental
path. Those are rows written before Apiary's `_time_format` fix; any row updated
now gets a current-format timestamp, so an unparseable one is by definition not
recently touched, and the backfill already has it.

### The event stream is a nudge, never a source of truth

With `source.wake` configured, the daemon's SSE stream wakes the loop so it does
not have to wait out `sync.interval`. Every fact replicated is read from the
database, never from the event payload, so a missed or duplicated signal costs
latency at worst. If the stream is unavailable the sink falls back to the
interval and says so.

A failed pass costs a pass, not the process: the loop backs off exponentially
and every pass re-reads the watermarks, so recovery carries no state across the
failure.

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
