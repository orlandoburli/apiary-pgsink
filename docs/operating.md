# Operating

## Metrics and health

```bash
pgsink sync -c pgsink.yaml --metrics 127.0.0.1:9847
```

| Endpoint | Meaning |
|---|---|
| `/metrics` | Prometheus exposition |
| `/healthz` | Liveness — the process is up |
| `/readyz` | Readiness — a pass has completed and the last one succeeded |

A sink with nothing to replicate is **healthy**. Readiness deliberately does not
mean "the last pass found rows": a probe that flaps whenever the source is quiet
is worse than no probe.

### The number to alert on

`pgsink_table_lag_seconds` — how far behind each table's replicated watermark
is. Alert on it rather than on pass counts, which say nothing about whether the
target is current.

```
pgsink_table_lag_seconds{instance="laptop",table="step_runs"} 4.500
```

Only tables with a timestamp cursor report lag. An integer watermark says which
row was last seen, not when, so those tables are absent here rather than given a
made-up number — use `pgsink_table_rows_written_total` for them.

| Metric | Type | |
|---|---|---|
| `pgsink_up` | gauge | 1 when a pass has completed and the last one succeeded |
| `pgsink_uptime_seconds` | gauge | |
| `pgsink_passes_total` | counter | |
| `pgsink_pass_errors_total` | counter | |
| `pgsink_rows_written_total` | counter | |
| `pgsink_rows_quarantined_total` | counter | alert if this moves at all |
| `pgsink_last_pass_duration_seconds` | gauge | |
| `pgsink_seconds_since_last_pass` | gauge | `-1` before the first pass |
| `pgsink_table_rows_written_total` | counter | per table |
| `pgsink_table_rows_quarantined_total` | counter | per table |
| `pgsink_table_lag_seconds` | gauge | per table, timestamp cursors only |

## Quarantine

A batch is written in one transaction, so a single row the target refuses fails
the whole batch — and if that row sits inside the watermark's range, it fails the
same batch on **every pass, forever**. The sink would stall on one value and
report only a repeating error.

Instead, a failed batch is retried row by row. Rows that fail on their own are
filed in `apiary_quarantine`, the rest of the batch lands, and the watermark
advances.

```sql
SELECT table_name, row_key, error_message, quarantined_at
FROM apiary.apiary_quarantine
ORDER BY quarantined_at DESC;
```

`row_data` holds the whole source row as `jsonb`, so a quarantined row can be
inspected — and replayed — without going back to the source.

A clean batch never pays for this: the isolating path runs only after a failure.

## Deployment

pgsink runs **on the daemon's host, as the daemon's user**. See
[Deployment](deployment.md) for why, and for the WAL permission rule that will
otherwise bite on first start.

=== "macOS (launchd)"

    Apiary itself runs under launchd on a Mac, and pgsink has to run beside it as
    the same user, so the shipped plist is a **LaunchAgent** rather than a
    LaunchDaemon: an agent runs in your login session, a daemon runs as root
    before login and is the wrong shape for this.

    ```bash
    sed "s/USERNAME/$(id -un)/g" deploy/com.orlandoburli.pgsink.plist \
      > ~/Library/LaunchAgents/com.orlandoburli.pgsink.plist
    # edit POSTGRES_DSN in it, then:
    chmod 600 ~/Library/LaunchAgents/com.orlandoburli.pgsink.plist
    launchctl load -w ~/Library/LaunchAgents/com.orlandoburli.pgsink.plist
    ```

    launchd has no `EnvironmentFile`, so the PostgreSQL DSN lives in the plist —
    which is why it must be `chmod 600`. It carries a password.

    ```bash
    launchctl list | grep pgsink                    # is it running
    tail -f ~/.apiary/logs/pgsink.log               # what is it doing
    launchctl unload -w ~/Library/LaunchAgents/com.orlandoburli.pgsink.plist
    ```

    Homebrew's PostgreSQL listens on `localhost:5432`, so a local target DSN is
    usually `postgres://$(id -un)@localhost/apiary`.

=== "systemd"

    Linux. The shipped unit is in `deploy/pgsink.service`.

    ```bash
    install -m0755 pgsink /usr/local/bin/pgsink
    install -m0640 -o apiary -g apiary pgsink.yaml /etc/pgsink/pgsink.yaml
    echo 'POSTGRES_DSN=postgres://user:pw@host/db' > /etc/pgsink/pgsink.env
    chmod 0600 /etc/pgsink/pgsink.env
    systemctl enable --now pgsink
    ```

    `User` and `Group` must match the Apiary daemon's.

=== "Docker"

    ```bash
    docker run --rm \
      --user "$(id -u apiary):$(id -g apiary)" \
      -v /home/apiary/.apiary:/home/apiary/.apiary \
      -v /etc/pgsink:/etc/pgsink:ro \
      -e POSTGRES_DSN \
      -p 127.0.0.1:9847:9847 \
      ghcr.io/orlandoburli/apiary-pgsink:latest \
      sync --config /etc/pgsink/pgsink.yaml --metrics 0.0.0.0:9847
    ```

    The data directory must be mounted **read-write** even though pgsink only
    reads: WAL needs to write the `-shm` index.

## Secrets

`${env:NAME}` expands in `source.dsn` and `target.dsn`, so a PostgreSQL password
never has to live in the config file. An unset variable fails at load with the
variable named, rather than resolving to an empty string that surfaces later as
the confusing "target.dsn is required".

## Restarting and resuming

The watermark lives in the target, in `apiary_sync_state`, so the sink carries no
state of its own. Stop it, move it to another host, start it again, and it
resumes from where the data actually is.

An interrupted pass repeats work rather than skipping it: the watermark advances
only after every batch has committed. Re-delivery is free because every write is
an idempotent upsert.
