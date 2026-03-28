# CLAUDE.md

## Build

```bash
go build ./cmd/outband/          # binary (NOT "go build .")
go build ./cmd/mockllm/          # mock LLM for testing
go test -race -count=1 ./...     # tests with race detector
docker compose build              # Docker images
./demo.sh                         # end-to-end demo (requires Docker)
```

The root package is `package outband` (a library), NOT `package main`. The binary entrypoint is `cmd/outband/main.go`.

## CI

CI workflow is `.github/workflows/ci.yml`. When the build path or flags change, **update ci.yml too** — it has its own `go build` command that must match.

## Rules

- On ANY failure — CI, build, test, runtime — read the relevant code and config files BEFORE theorizing. Do not guess. Do not assume. Open the file, read the line, trace the actual execution path.
- Do not add workarounds (cache-busting, env var toggles, defensive checks) for problems you haven't root-caused. Find the actual broken line first.
- When the package layout changes, grep for every build/run command across: `ci.yml`, `Dockerfile`, `docker-compose.yml`, `demo.sh`, `Makefile`, and `README.md`. Update all of them in the same commit.
