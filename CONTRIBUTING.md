# Contributing to Soroq CLI

Thanks for helping improve Soroq.

## Development Setup

Requirements:

- Go matching `backend/go.mod`
- Git
- Android tooling only when working on Android artifact inspection, release, or patch flows

Run the CLI locally:

```bash
cd backend
go run ./cmd/soroq --help
```

Run tests:

```bash
cd backend
go test ./cmd/soroq ./internal/...
```

Build a local binary:

```bash
cd backend
go build -o soroq ./cmd/soroq
./soroq --help
```

## Pull Requests

Please keep PRs focused. Good contribution lanes include:

- CLI help, terminal UI, and error messages
- Android artifact inspection
- Android release and patch packaging
- Installer improvements
- Cross-platform build and release automation
- Tests for any changed behavior

Do not commit local credentials, operator tokens, private keys, `.soroq` state, build outputs, or downloaded release archives.
