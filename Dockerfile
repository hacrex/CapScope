# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
# go-sqlite3 uses cgo, so the build stage needs a C toolchain.
RUN apk add --no-cache gcc musl-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -tags sqlite_omit_load_extension -ldflags="-s -w" -o /out/capscope ./cmd/capscope

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/capscope /usr/local/bin/capscope
ENV LISTEN=:8080 DB_PATH=/data/capscope.db
VOLUME /data
EXPOSE 8080
# Runs as root on purpose: reading /var/run/docker.sock needs it. See docs/CAPROVER.md for the
# socket-proxy setup that removes this requirement.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["capscope"]
