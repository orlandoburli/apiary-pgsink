# Quickstart

Five minutes from nothing to a PostgreSQL copy of your Apiary history that stays
current.

Assumes [pgsink is installed](installation.md) and you have a PostgreSQL to
write to. If you do not, this will do for a look around:

```bash
docker run -d --name pgsink-target \
  -e POSTGRES_USER=pgsink -e POSTGRES_PASSWORD=pgsink -e POSTGRES_DB=pgsink \
  -p 5432:5432 postgres:17-alpine
```

## 1. Write a config

`pgsink.yaml`, next to your Apiary config:

```yaml
source:
  dsn: sqlite:///Users/you/.apiary/apiary.db
  instance: laptop                              # names this Apiary in the target
  wake: unix:///Users/you/.apiary/apiary.sock   # optional: low-latency nudge

target:
  dsn: ${env:POSTGRES_DSN}
  schema: apiary

defaults:
  # Whole agent transcripts. Most of the bytes, and where anything sensitive
  # lives — off unless you ask for them.
  exclude_columns: [input_prompt, output_text, full_output]

tables:
  # Roughly 10KB of stream text per row, and Apiary prunes them anyway.
  task_logs: {enabled: false}
  service_logs: {enabled: false}
```

`instance` is worth a moment's thought: it identifies this Apiary in the target
and is part of every primary key, so several installations can share one
database. Changing it later means reloading.

```bash
export POSTGRES_DSN='postgres://pgsink:pgsink@localhost:5432/pgsink'
```

The DSN carries a password, so keep it in the environment rather than the file.
`${env:NAME}` expands in `source.dsn` and `target.dsn`, and an unset variable
fails at load with the variable named.

## 2. Check the config against your database

```bash
pgsink doctor -c pgsink.yaml --db ~/.apiary/apiary.db
```

This is the step that saves you time. It resolves the whole config against your
real schema and reports a filter on a column your Apiary does not have, an
injected field colliding with a real one, or a table name you misspelt — before
anything is written.

## 3. Create the target tables

```bash
pgsink migrate -c pgsink.yaml --dry-run   # read the DDL first
pgsink migrate -c pgsink.yaml
```

Additive only, and applied in one transaction.

## 4. Load the history

```bash
pgsink backfill -c pgsink.yaml
```

```
task_executions                    412 rows  38ms
step_runs                          908 rows  61ms
execution_events                  3120 rows  102ms
…
4440 rows in 291ms
```

Safe to re-run: every write is an idempotent upsert.

## 5. Follow

```bash
pgsink sync -c pgsink.yaml --metrics 127.0.0.1:9847
```

It runs until interrupted. To keep it running, install the
[launchd agent or systemd unit](operating.md#deployment).

## 6. Ask it something

```sql
SELECT agent_id,
       count(*)                        AS runs,
       round(sum(cost_usd)::numeric, 2) AS cost,
       sum(total_tokens)               AS tokens
FROM apiary.task_executions
WHERE status = 'success'
  AND created_at > now() - interval '30 days'
GROUP BY agent_id
ORDER BY cost DESC;
```

Per-step detail lives in `apiary.step_runs` — tokens, cost and wall-clock
attribution per workflow step, which is usually the more interesting table:

```sql
SELECT step_id,
       count(*)                                         AS runs,
       round(avg(total_tokens))                         AS avg_tokens,
       round(avg(time_tool_wait_ms) / 1000.0, 1)        AS avg_tool_wait_s
FROM apiary.step_runs
WHERE state = 'passed'
GROUP BY step_id
ORDER BY runs DESC;
```

## Two things to know

**Cost arrives late.** Apiary inserts an execution row at dispatch with zero
cost and fills in the tokens and cost at completion. pgsink follows that second
write — that is most of what it does — so a row you read mid-run legitimately
shows zero. See [the table catalog](catalog.md#why-classes-exist).

**The sink is an archive.** Apiary prunes old logs and can delete tasks; a
cursor-based follower never observes a delete, so PostgreSQL keeps rows Apiary
has dropped. For reporting that is usually what you want — but it is a choice,
not an accident.

Next: [Commands](commands.md) for what each one does, or
[Operating](operating.md) for metrics, health checks and the quarantine.
