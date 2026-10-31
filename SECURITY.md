# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| v0.6    | ✅        |
| < v0.6  | ❌        |

## Reporting a Vulnerability

Please do not disclose security issues publicly.

Email security concerns to the maintainer via GitHub Discussions or open a private advisory through GitHub's "Report a vulnerability" feature.

Include:
- CapScope version
- Deployment method (Docker Compose / CapRover / raw Docker)
- Steps to reproduce
- Impact assessment

## Security Notes

- CapScope requires `AUTH_PASS` to start; it refuses to run without a password unless `ALLOW_NO_AUTH=true` is explicitly set.
- CapScope mounts `/var/run/docker.sock`, which is root-equivalent on the host. Use a read-only socket proxy (`tecnativa/docker-socket-proxy`) and HTTPS in production.
- Secrets (JWTs, bearer tokens, passwords, AWS keys, PEM keys) are masked on ingest via best-effort redaction. Redaction is a heuristic — do not rely on it as your only secret protection.
- Snapshots never store environment variables or labels.
- One shared Basic auth credential. Per-app permissions and OIDC are on the roadmap.

## Disclosure Policy

We will acknowledge receipt within 7 business days and provide a timeline for a fix. Public disclosure will be coordinated with the reporter.
