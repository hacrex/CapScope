# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
# cgo (go-sqlite3) built against musl with a static link, so the final image
# can be scratch: no OS layer, no shell, no package manager.
RUN apk add --no-cache gcc musl-dev binutils ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -tags "sqlite_omit_load_extension fts5" \
    -ldflags="-s -w -extldflags=-static" -o /out/capscope ./cmd/capscope
# Refuse to ship a dynamically linked binary: scratch has no loader.
# (scanelf prints a line even for static files, so check the INTERP segment instead.)
RUN if readelf -l /out/capscope | grep -q INTERP; then \
      echo "binary is dynamically linked:" >&2; readelf -l /out/capscope | grep INTERP >&2; exit 1; \
    fi
RUN mkdir -p /out/tmp && : > /out/tmp/.keep # /tmp for SQLite temp files; .keep keeps it real on COPY

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tmp /tmp
COPY --from=build /out/capscope /capscope
ENV LISTEN=:8080 DB_PATH=/data/capscope.db
VOLUME /data
EXPOSE 8080
# Runs as root on purpose: reading /var/run/docker.sock needs it. See docs/CAPROVER.md for the
# socket-proxy setup that removes this requirement.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s CMD ["/capscope", "--healthcheck"]
ENTRYPOINT ["/capscope"]
