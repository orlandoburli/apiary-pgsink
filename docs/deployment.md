# Deployment

pgsink runs **on the same host as the Apiary daemon**. It reads the daemon's
SQLite database directly and takes a low-latency nudge from the daemon's event
stream; only the PostgreSQL connection crosses the network.

## Why same host

Apiary serves `/events` and `/events/stream` over a **Unix domain socket**, not
TCP. A process on another machine can reach neither the socket nor the database
file, so a remote deployment would need a new authenticated read endpoint inside
Apiary. That may come later; today, same host.

## Process identity

!!! warning "SQLite in WAL mode has no true read-only reader"

    A process opening the database — **even with `mode=ro`** — must be able to
    write the `-shm` wal-index file, or create it in the directory containing
    the database. A pgsink running as its own hardened user with only read bits
    fails at startup with `unable to open database file`, which reads like a
    path bug and is not one.

Run pgsink as the Apiary daemon's user:

```ini
# /etc/systemd/system/pgsink.service
[Service]
User=apiary
Group=apiary
ExecStart=/usr/local/bin/pgsink sync --config /etc/pgsink/pgsink.yaml
```

Or put both processes in a group with write access to the data directory:

```bash
chgrp -R apiary ~/.apiary
chmod -R g+w ~/.apiary
```

### Do not reach for `immutable=1`

It silences the error and corrupts your results. `immutable=1` promises SQLite
that the file cannot change while the daemon is actively writing to it, so reads
go quietly wrong instead of failing loudly. pgsink does not accept it.

## Platforms

pgsink runs on **macOS and Linux**, on amd64 and arm64 — everything it depends
on is portable: a Unix socket, SQLite's WAL, and a PostgreSQL connection. Both
the binary and the container image are static, because `modernc.org/sqlite` and
`pgx` are pure Go and CGO stays off.

Only the *service wrapper* differs. `deploy/pgsink.service` is a systemd unit for
Linux; `deploy/com.orlandoburli.pgsink.plist` is a launchd agent for macOS. See
[Operating](operating.md#deployment).

On a Mac the permission rule above is usually satisfied for free: the Apiary
daemon runs in your own login session, and so does a LaunchAgent.

## Containers

A container works, but it must bind-mount the Apiary data directory and run as
the daemon's user — the permission rule above does not change. The systemd unit
is the simpler default.

## What pgsink never does

It opens the database with `mode=ro` and `query_only(true)`, through SQLite's
`file:` URI form so both are actually honoured. It creates no tables, sets no
pragmas that persist, and refuses to create a database that does not already
exist.
