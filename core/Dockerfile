# syntax=docker/dockerfile:1
# gex-core — the Go core as one static binary. Same promise as the native
# builds (architecture.md §8/§9: pure Go, no CGO, one artifact per OS); the
# container is simply the linux/amd64 build of it. The C# edge stays native
# (Windows-side, next to TWS) and connects over the mapped 7878 port; the
# nightly GARCH fit job lives in ops/.

FROM golang:1.27-alpine AS build
WORKDIR /src

# dependency layer: cached unless go.mod/go.sum change (modernc.org/sqlite is
# the only direct dependency and the only slow download)
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
RUN go build -trimpath -ldflags="-s -w" -o /out/gexctl ./cmd/gexctl

FROM alpine:3.20
RUN addgroup -S gex && adduser -S gex -G gex \
 && mkdir -p /data && chown gex:gex /data
COPY --from=build /out/gexctl /usr/local/bin/gexctl

# non-root; the SQLite file lives in /data (mount a named volume — WAL does
# not like bind-mount filesystems)
USER gex
WORKDIR /data
VOLUME /data
EXPOSE 8787

ENTRYPOINT ["gexctl"]
# default: boot disconnected into saved-snapshot mode (press Connect in the
# UI); docker-compose overrides with --connect for the live synthetic feed.
# The address must be 0.0.0.0 inside a container — the 127.0.0.1 default is
# unreachable through a port mapping.
CMD ["serve", "--addr", "0.0.0.0:8787", "--db", "/data/gex.db"]
