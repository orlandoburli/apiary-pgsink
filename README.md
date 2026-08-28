# apiary-pgsink

Replicate an [Apiary](https://github.com/orlandoburli/apiary) SQLite database
into PostgreSQL — backfill the history, then follow it — with per-table filters
and injected fields.

Standalone by construction: its own repository, its own release cadence, and no
Go dependency on Apiary in either direction. It reads the daemon's database; it
is not a plugin and does not run inside the daemon.

```bash
brew install --cask orlandoburli/tap/pgsink

pgsink doctor      # check the table catalog against a live Apiary database
pgsink migrate     # create or alter the target tables
pgsink backfill    # load history
pgsink sync        # follow forever
```

**[Installation](https://orlandoburli.com.br/apiary-pgsink/installation/)** ·
**[Quickstart](https://orlandoburli.com.br/apiary-pgsink/quickstart/)** —
five minutes from nothing to a PostgreSQL copy that stays current.

Backfill and sync are the same pipeline. Only the starting watermark and the
stop condition differ, so the backfill path is exercised by every test the
follower has.

## Status

All four commands work, with Prometheus metrics, health checks and a poison-row
quarantine. Not yet tagged for release.

## Requirements

- **macOS or Linux**, amd64 or arm64. A launchd agent and a systemd unit are
  both shipped in `deploy/`.
- Runs **on the same host as the Apiary daemon**, as the daemon's user or in a
  group with write access to its data directory. SQLite in WAL mode has no true
  read-only reader — even a reader must be able to write the `-shm` wal-index
  file. See [docs/deployment.md](docs/deployment.md).
- PostgreSQL 13 or newer as the target. Only this connection crosses the
  network.

## Documentation

<https://orlandoburli.com.br/apiary-pgsink>

## License

Licensed under the [BSD 3-Clause License](LICENSE) — the same terms as
[Apiary](https://github.com/orlandoburli/apiary). No commercial license is
required; see [COMMERCIAL.md](COMMERCIAL.md).
