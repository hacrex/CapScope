# CapScope — read-only Docker container log viewer
# CapRover one-click deploy image.
#
# Build:
#   CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -tags "sqlite_omit_load_extension fts5" -o capscope-linux-amd64 ./cmd/capscope
#
# CapRover deploy:
#   1. Push this image to your registry
#   2. Create a One-Click App in CapRover using the image
#   3. Mount /var/run/docker.sock → /var/run/docker.sock (read-only)
#   4. Mount a volume for /data (SQLite persistence)
#   5. Set env: AUTH_PASS, ALLOW_NO_AUTH=false (recommended)

FROM debian:stable-slim

# CA certificates for HTTPS (ntfy alerts, update checks)
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

COPY capscope-linux-amd64 /capscope
RUN chmod +x /capscope

# CapRover sets PORT automatically
ENV LISTEN=0.0.0.0:${PORT:-8080}
ENV DB_PATH=/data/capscope.db
ENV DOCKER_HOST=unix:///var/run/docker.sock
# Uncomment for production auth:
# ENV AUTH_PASS=your-secure-password
# ENV ALLOW_NO_AUTH=false
# ENV LOG_LEVEL=info

VOLUME ["/data", "/var/run/docker.sock"]
EXPOSE ${PORT:-8080}

ENTRYPOINT ["/capscope", "serve"]
