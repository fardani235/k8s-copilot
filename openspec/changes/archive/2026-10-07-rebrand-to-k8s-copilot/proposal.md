## Why

The tool identifies itself as `k2stui` while the repository, the git remote, and
the product intent are all `k8s-copilot`. The name `k2stui` also differs from the
sibling project `k8stui` (a separate Python/Textual tool) by a single character,
which makes code, docs, and conversation ambiguous. The founding change
(`add-k2stui`) is now archived and nothing has been released, so this is the last
cheap moment to align the internal identity before anyone depends on the module
path, environment variables, or file locations.

## What Changes

- Rename the Go module path `github.com/fardani235/k2stui` -> `github.com/fardani235/k8s-copilot` and update every import.
- Rename the command directory `cmd/k2stui` -> `cmd/k8s-copilot` and the built binary `bin/k2stui` -> `bin/k8s-copilot`.
- **BREAKING** Rename the environment prefix `K2STUI_*` -> `K8S_COPILOT_*` (including `K2STUI_API_KEY`, the live-test variables, and every documented variable).
- **BREAKING** Move the config file `~/.config/k2stui/config.yaml` -> `~/.config/k8s-copilot/config.yaml` and the audit trail `~/.local/state/k2stui/audit.jsonl` -> `~/.local/state/k8s-copilot/audit.jsonl`.
- Rename all user-visible strings: `version`/error prefixes, the client user-agent, the OpenRouter `X-Title`, the TUI header and help, the terminal-too-small message, and the agent system prompt.
- Update current documentation and build files: `README.md`, `docs/*.md`, the "Unreleased" `CHANGELOG.md` entry, `Makefile`, and `.gitignore`.
- **Clean break**: no compatibility shims, no fallback reads of the old environment prefix or paths, no migration of the old audit trail. The old local state is left in place and simply ignored.
- **Non-goal**: no visual identity work — no theme, colors, or logo. The name is the only thing that changes.
- **Not touched**: the sibling projects `k8stui` and `volc-tui`, and the archived `openspec/changes/archive/2026-10-07-add-k2stui/` record.

## Capabilities

None. This change makes no spec-level behavior change: it renames the product's
identity (module, binary, environment variables, file locations, display
strings) while every capability's observable behavior stays identical. The
change therefore sets `skip_specs: true` in `.openspec.yaml` instead of
inventing a requirement to satisfy validation.

### New Capabilities
<!-- None: identity is not a behavior contract. -->

### Modified Capabilities
<!-- None: resource-browser, agent-copilot, and audit-trail requirements are unchanged. -->

## Impact

- **Source and tests**: `go.mod`, `cmd/k2stui/` (moved), every file importing the module (all `internal/*` packages and their tests), `internal/config/config.go` and `config_test.go`, `internal/audit/audit.go`, `internal/tui/*` (header, help, messages), `internal/agent/agent.go` (system prompt), `internal/llm/http.go` and `openai.go` (key env, `X-Title`), `internal/guard/guard_test.go` (a hardcoded `cmd/` path).
- **Build**: `Makefile` (binary and `go run` paths, `K2STUI_LIVE`), `.gitignore`.
- **Docs**: `README.md`, `docs/architecture.md`, `docs/configuration.md`, `docs/decisions.md`, `docs/security.md`, `docs/testing.md`, `CHANGELOG.md`.
- **Dependencies**: none added or removed.
- **Runtime contract**: environment variables and config/state paths change. This is intentional and unimplemented as a compatibility layer; there are no external users and no releases to migrate.
- **Out of scope**: capability behavior, the archived change record, and the sibling projects.
