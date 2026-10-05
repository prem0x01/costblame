# Contributing

Open work is tracked in the [roadmap issue (#24)](https://github.com/prem0x01/costblame/issues/24); each item links to a detailed issue.

## Branches

`main` is always releasable. Work happens on area branches cut from `main`:

| Branch | Covers | Issue label |
|---|---|---|
| `area/backend` | scoring, detection, ingestion, storage, engine | `backend` |
| `area/ui` | web UI and TUI | `ui` |
| `area/cicd` | build, release, containers, CI | `cicd` |
| `area/security` | auth hardening, abuse resistance | `security` |
| `area/docs` | README, config docs, examples | `documentation` |
| `area/tests` | test coverage and tooling | `tests` |

For a single issue, branch off the area branch (for example `backend/12-engine-retries`) or commit to the area branch directly, then open a pull request into `main`. Do not force-push `main` or the area branches.

## Commits

- Short imperative subject; explain the *why* in the body.
- Reference the issue: `Refs #N` while work is in progress, `Fixes #N` when the commit completes it.

## Before you push

```bash
gofmt -l .                              # should print nothing
go vet ./...
CGO_ENABLED=1 go test -race ./...
```
