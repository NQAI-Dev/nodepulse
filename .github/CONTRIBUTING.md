# Contributing to NodePulse

Thanks for taking the time to contribute.

## Ground Rules
- Be respectful. Assume good intent.
- Keep the dependency graph empty. The standard library is enough for 95% of what we need.
- Bias toward deletion. If a feature can live behind a feature flag, it should not exist yet.
- Every commit must compile and pass `go test ./...`.

## Development Setup
1. Install Go 1.22+.
2. Fork and clone the repository.
3. Run `go test ./...` — the suite must be green before you start.
4. Build the binaries: `make build` (or `go build ./cmd/agent ./cmd/server`).

## Coding Conventions
- **Comments**: explain *why*, not *what*. Russian is welcome in commit messages; English in code comments.
- **Error handling**: wrap with `%w`; do not swallow.
- **Memory**: profile with `pprof`. RSS at idle should stay under 5 MB for the agent.
- **Tests**: every non-trivial function gets a unit test. Use `testdata/` for fixtures.
- **Naming**: `pkg/<area>/<file>.go`; no `utils` package.

## Commit Messages
Write in Russian, descriptive style:
- `feat(collector): добавить парсинг /proc/net/dev`
- `fix(store): устранить deadlock в CreateIncident`
- `chore: обновить Go до 1.22`

## Pull Requests
- Open a discussion (issue) first for non-trivial changes.
- Keep PRs focused — one logical change per PR.
- Update tests, documentation, and `CHANGELOG.md` (when it exists).

## Reporting Bugs
Use the **Bug Report** issue template. Include the agent and server versions, the OS / kernel, and the exact command or HTTP request that triggers the bug.

## Reporting Security Issues
See `SECURITY.md`. **Do not** file a public issue for security bugs.
