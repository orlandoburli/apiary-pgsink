# Build
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# CGO off: modernc.org/sqlite and pgx are both pure Go, so the binary is static
# and the runtime image needs nothing but certificates.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/pgsink ./cmd/pgsink

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/pgsink /usr/local/bin/pgsink

# pgsink reads the Apiary daemon's SQLite database directly, and SQLite in WAL
# mode has no true read-only reader: even a reader must be able to write the
# -shm wal-index file. Run as the daemon's user and bind-mount its data
# directory read-write, or this fails at startup with "unable to open database
# file". The UID here is a placeholder — override it to match your host.
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/pgsink"]
CMD ["sync", "--config", "/etc/pgsink/pgsink.yaml"]
