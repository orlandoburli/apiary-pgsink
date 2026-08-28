# Installation

pgsink runs **on the same host as the Apiary daemon, as the daemon's user**.
That is not a preference — see [Deployment](deployment.md#process-identity) for
why, and read it before you pick an install method, because it decides which one
fits.

=== "Homebrew"

    The same tap Apiary itself installs from:

    ```bash
    brew install --cask orlandoburli/tap/pgsink
    ```

    Upgrades with `brew upgrade --cask pgsink`.

=== "Binary"

    Download from [releases](https://github.com/orlandoburli/apiary-pgsink/releases),
    for macOS or Linux on amd64 or arm64:

    ```bash
    VERSION=0.1.1
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

    curl -sSLO "https://github.com/orlandoburli/apiary-pgsink/releases/download/v${VERSION}/pgsink_${VERSION}_${OS}_${ARCH}.tar.gz"
    curl -sSLO "https://github.com/orlandoburli/apiary-pgsink/releases/download/v${VERSION}/checksums.txt"
    shasum -a 256 -c checksums.txt --ignore-missing

    tar -xzf "pgsink_${VERSION}_${OS}_${ARCH}.tar.gz"
    sudo install -m0755 pgsink /usr/local/bin/pgsink
    ```

    The archive also carries the service files — `deploy/pgsink.service` for
    systemd and `deploy/com.orlandoburli.pgsink.plist` for launchd — and a
    worked `examples/pgsink.yaml`.

    On macOS an unsigned binary is quarantined by Gatekeeper. The Homebrew cask
    clears it for you; downloading by hand does not:

    ```bash
    xattr -dr com.apple.quarantine /usr/local/bin/pgsink
    ```

=== "Container"

    ```bash
    docker pull ghcr.io/orlandoburli/apiary-pgsink:0.1.1
    ```

    Multi-arch, 37MB, static. It must bind-mount the Apiary data directory
    **read-write** and run as the daemon's user — see
    [Operating](operating.md#deployment).

=== "From source"

    ```bash
    git clone https://github.com/orlandoburli/apiary-pgsink
    cd apiary-pgsink && make build     # → bin/pgsink
    ```

    Go 1.26 or newer. No CGO, no system libraries.

## Verify it

```bash
pgsink --version
pgsink doctor --db ~/.apiary/apiary.db
```

`doctor` reads your Apiary database and checks it against pgsink's table
catalog. A clean result looks like this:

```
catalog     24 tables, schema_version 1, apiary >=0.12.0
database    /Users/you/.apiary/apiary.db

schema      no drift — the catalog matches all 24 tables
```

Warnings are normal on an older Apiary — a table that does not exist yet is
skipped, not a failure. Errors mean replication would be incorrect; see
[Table catalog](catalog.md#drift).

## What you also need

- **An Apiary daemon** on this host, version 0.12.0 or newer.
- **PostgreSQL 13 or newer** as the target. This is the only thing that has to
  be reachable over the network; everything else is local.

Next: [Quickstart](quickstart.md).
