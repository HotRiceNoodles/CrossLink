# Dependency Policy

How CrossLink manages its Go module dependencies and toolchain versions.

## Toolchain

- The `go` directive in `go.mod` is the single source of truth; CI pins to it
  via `go-version-file: go.mod`.
- We track the latest two Go minor releases. Renovate opens a dedicated PR when
  a new minor is available; it is merged after CI is green across all jobs.
- Never use language/stdlib features newer than the `go` directive.

## Update automation (Renovate)

[Renovate](https://docs.renovatebot.com/) (`renovate.json`) automates dependency
updates:

| Class | Policy |
|---|---|
| Security alerts | Vulnerability alerts are enabled; fix PRs are prioritized |
| `go.opentelemetry.io/*` | Upgraded as one group (they release in lockstep) |
| `gorm.io/*` | Upgraded as one group |
| `github.com/gin-gonic/*` | Upgraded as one group |
| Other direct patch/minor | Aggregated into one weekly PR (Monday) |
| Direct major bumps | Individual PRs, labeled `major-update` |
| `github.com/tjfoc/gmsm` | **Crypto-sensitive**: individual PR, 14-day minimum release age, careful review required |

`go mod tidy` runs automatically after every update (`gomodTidy` post-update option).

## Review requirements

- All Renovate PRs go through the same CI as human PRs (lint, test, spec/sdk
  checks, build smoke).
- **Crypto-sensitive dependencies** (`github.com/tjfoc/gmsm` and anything under
  the GM/SM2/SM3/SM4 code paths): review the upstream diff and release notes,
  and run `internal/crypto` tests explicitly before merging:
  ```bash
  go test ./internal/crypto/... -v
  ```
- Major bumps of the web framework (Gin), ORM (GORM), or migration library
  (golang-migrate) additionally require a smoke run against a real PostgreSQL:
  `make test-integration`.

## Manual intervention

Run these when a Renovate PR conflicts or the dashboard shows an error:

```bash
go mod tidy
go build ./...
make test
```

If a dependency must be pinned or excluded, do it in `renovate.json` (with a
comment in the PR describing why), not by hand-editing `go.sum`.
