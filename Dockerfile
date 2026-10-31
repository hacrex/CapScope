# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
# modernc.org/sqlite is pure Go; no C compiler needed. ca-certificates for
# HTTPS outbound from the scratch image.
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -tags "sqlite_omit_load_extension fts5" \
    -ldflags="-s -w" -o /out/capscope ./cmd/capscope
RUN mkdir -p /out/tmp && : > /out/tmp/.keep # /tmp for SQLite temp files

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tmp /tmp
COPY --from=build /out/capscope /capscope
ENV LISTEN=:8080 DB_PATH=/data/capscope.db
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s CMD ["/capscope", "--healthcheck"]
ENTRYPOINT ["/capscope"]
