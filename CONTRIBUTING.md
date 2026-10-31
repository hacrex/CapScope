# Contributing to CapScope

Thanks for contributing! CapScope is a small Go project with no frontend build step — the UI is a single embedded HTML file (`internal/web/static/index.html`).

## How to contribute

1. **Fork the repo** and create a branch from `main`.
2. **Run the tests** before and after your change:

   ```bash
   make test
   ```

3. **Run go vet**:

   ```bash
   go vet ./...
   ```

4. **Build locally** to verify:

   ```bash
   make build
   ```

5. **Open a pull request** describing what changed and why.

## Development

| Command | Purpose |
|---------|---------|
| `make build` | Build the `capscope` binary |
| `make run` | Run locally with no auth (port 127.0.0.1:8080) |
| `make demo` | Run with fake data |
| `make test` | Vet + run test suite with race detector |
| `make docker` | Build `capscope:dev` image |

The UI is embedded in the Go binary via `internal/web/static/index.html`. Edit that file for UI/UX changes; no npm build step is needed.

## Style

- Go: standard `gofmt` + `go vet`
- Commit messages: conventional style (`feat(ui): ...`, `fix(api): ...`, `docs: ...`)

## Code of conduct

Be respectful and constructive. This is a small project maintained for self-hosters and homelabbers.
