# Contributing to CrossLink

Thank you for your interest in contributing to CrossLink! This document covers the process and guidelines for contributing.

## Code of Conduct

By participating, you agree to uphold our [Code of Conduct](CODE_OF_CONDUCT.md).

## How to Contribute

### Bug Reports

1. Search [existing issues](../../issues) to avoid duplicates.
2. Open a new issue with:
   - Clear title and description
   - Steps to reproduce
   - Expected vs actual behavior
   - Relevant logs or screenshots

### Feature Requests

1. Check [existing issues](../../issues) and [discussions](../../discussions) first.
2. Open an issue with the `feature` label describing the use case and proposed solution.

### Pull Requests

1. **Fork** the repository and create a branch from `main`.
2. Make your changes with clear, focused commits.
3. Ensure all tests pass: `go test ./...`
4. If adding a feature, include tests.
5. Submit a PR with a clear description of the change and motivation.

## Development Setup

### Prerequisites

- Go 1.26+
- PostgreSQL 14+
- Redis 7+
- Node.js 18+ (for frontend)

### Backend

```bash
# Configure
cp configs/config.yaml.example configs/config.yaml
# Edit config.yaml with your database/redis settings

# Run migrations (automatic on startup) and start the server
go run ./cmd/server
```

### Frontend

```bash
cd web
npm install
npm run dev
```

### Running Tests

```bash
go test ./...
```

## Coding Guidelines

- Follow standard Go conventions ([Effective Go](https://go.dev/doc/effective_go)).
- Keep changes minimal and focused — one PR per concern.
- Match existing code style in the files you modify.
- Write tests for new functionality.
- Do not add features beyond what was requested or refactor unrelated code.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).