## 1. Module and command layout

- [x] 1.1 Change the module path in `go.mod` to `github.com/fardani235/k8s-copilot` and update every import under `cmd/` and `internal/`; verify `go build ./...` succeeds and `grep -rn 'github.com/fardani235/k2stui' --include='*.go' .` is empty
- [x] 1.2 Move the command directory with `git mv cmd/k2stui cmd/k8s-copilot`; verify `go build ./...` and `go vet ./...` succeed and `go run ./cmd/k8s-copilot version` runs
- [x] 1.3 Update the hardcoded command-path literal in `internal/guard/guard_test.go` (currently `cmd/k2stui/main.go`); verify `go test ./internal/guard/...` passes

## 2. Build and ignore files

- [x] 2.1 Update `Makefile` (output `bin/k8s-copilot`, `go run ./cmd/k8s-copilot`, live-test env `K8S_COPILOT_LIVE`) and `.gitignore` (`/k2stui` -> `/k8s-copilot`); verify `make build` produces `bin/k8s-copilot`

## 3. Runtime contract (clean break, no compatibility shims)

- [x] 3.1 Rename the `K2STUI_*` environment prefix to `K8S_COPILOT_*` in `internal/config/config.go` (every variable plus the help text) and the `K2STUI_API_KEY` default in `internal/llm/http.go`; verify `go test ./internal/config/... ./internal/llm/...` passes
- [x] 3.2 Change the default config path and audit-trail directory to the `k8s-copilot` names in `internal/config/config.go` and `internal/audit/audit.go`; verify `go test ./internal/config/... ./internal/audit/...` passes and no path string contains `k2stui`
- [x] 3.3 Rename the test/documentation-only variables `K2STUI_LIVE` and `K2STUI_LIVE_DEPLOYMENT` in `internal/tools/live_test.go`, `Makefile`, and `docs/testing.md`; verify `grep -rn 'K2STUI' .` finds nothing outside the frozen `openspec/changes/archive/` and this change's own artifacts (which name the old prefix by necessity)

## 4. User-visible strings

- [x] 4.1 Replace `k2stui` in `cmd/k8s-copilot/main.go` — `version` output, error prefixes, audit messages, and the client user-agent (`k8s-copilot/<version>`); verify `go run ./cmd/k8s-copilot version` prints `k8s-copilot <version>`
- [x] 4.2 Update the TUI strings in `internal/tui/*` (header, help, terminal-too-small message, focus hints); verify `go test ./internal/tui/...` passes
- [x] 4.3 Update the agent system prompt and event notices in `internal/agent/agent.go` and `internal/agent/mutate.go`; verify `go test ./internal/agent/...` passes
- [x] 4.4 Update the OpenRouter `X-Title` header in `internal/llm/openai.go`; verify `go test ./internal/llm/...` passes
- [x] 4.5 Sweep any remaining `k2stui` string literals in `internal/approval/`, `internal/kube/`, and `internal/tools/`; verify `go test ./...` passes and no `k2stui` string literal remains (grep)

## 5. Documentation

- [x] 5.1 Rewrite `README.md` — title, hero diagram, every `k2stui`/`K2STUI_*` reference, and the archived-change link; verify no product-name `k2stui` remains (the archived change's name and path are retained) and every relative link resolves
- [x] 5.2 Rename references in `docs/architecture.md`, `docs/configuration.md`, `docs/decisions.md`, `docs/security.md`, and `docs/testing.md`; verify `grep -rn 'k2stui\|K2STUI' docs/ README.md CHANGELOG.md` shows no product-name reference (only the archived-change path in `docs/decisions.md`)
- [x] 5.3 Update the `CHANGELOG.md` "Unreleased" entry (including the module and binary line) to the new name; verify it no longer contains `k2stui`

## 6. Verification

- [x] 6.1 Run `make check` (vet plus the full test suite) and confirm it passes
- [x] 6.2 Confirm the clean break: `k8s-copilot config` prints the `k8s-copilot` config/audit paths, and a fresh run creates/writes `~/.local/state/k8s-copilot/audit.jsonl` while the old `~/.config/k2stui/` and `~/.local/state/k2stui/` are ignored
- [x] 6.3 Confirm scope: surviving `k2stui`/`K2STUI` occurrences are confined to the frozen `openspec/changes/archive/`, this change's own artifacts, `.opencode/MEMORY.md`, and historical `add-k2stui` references in current docs; `k8stui` and `volc-tui` are unchanged

## Implementation notes

- Specs and design were deliberately skipped for this change: the rename changes no capability behavior (`skip_specs: true` in `.openspec.yaml`) and introduces no architectural decision, dependency, data-model change, migration, or ambiguity. See `proposal.md`.
- This is a clean break: no fallback reads of the old environment prefix or paths and no migration of the old audit trail. The old local state is left in place and ignored.
