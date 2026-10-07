# Changelog

## Unreleased — first implementation (`openspec/changes/archive/2026-10-07-add-k2stui`)

Greenfield: the repository previously held only the OpenSpec documents.

### Added

- **Go module** `github.com/fardani235/k8s-copilot`, single binary `cmd/k8s-copilot`
  (Bubble Tea / Lip Gloss / Bubbles, client-go).
- **Resource browser** (`internal/kube`, `internal/tui`): connects via the
  standard kubeconfig rules (`--context`, `--kubeconfig`, `-n` overrides);
  generic discovery of every listable type including CRDs; kubectl-style
  columns from the API server's Table printer with a generic fallback;
  namespace scoping and all-namespaces; detail view (key fields + YAML); pod
  logs (container switch, follow, previous instance, bounded buffer); events
  (most recent last); filter; manual and periodic refresh; explicit empty,
  permission-denied, discovery-failed and terminal-too-small states.
- **Copilot** (`internal/agent`, `internal/llm`): a locally owned, bounded
  agent loop behind a one-method provider interface. Providers: Anthropic,
  OpenAI, OpenRouter, any OpenAI-compatible endpoint; a scripted stub for
  tests. Browser focus is injected into every request.
- **Typed tool registry** (`internal/tools`): read tools `list_resources`,
  `get_resource`, `describe_resource`, `get_logs`, `get_events`; gated mutate
  tools `scale`, `rollout_restart`, `set_labels`, `set_annotations`. Strict
  argument decoding. Nothing else exists; out-of-scope requests are refused
  with an explanation.
- **Approval gate** (`internal/approval`, `internal/tui/modal.go`):
  server-side dry-run before the human is asked; a dialog with action, target,
  cluster, before→after, dry-run verdict, reversibility, warnings and the exact
  request; approve (`ctrl+y`, `enter`) / reject (`ctrl+n`) / edit
  (re-validated) / hide. Blocks
  indefinitely; no timeout or default.
- **Audit trail** (`internal/audit`): append-only hash-chained JSONL at
  `~/.local/state/k8s-copilot/audit.jsonl`; `k8s-copilot audit show`, `k8s-copilot audit
  verify`, and an in-app view (`A`). An approval is recorded before the
  change is sent and its result after. Fail-closed: no working trail, no
  proposals.
- **Configuration** (`internal/config`): defaults < file < `K8S_COPILOT_*` < flags;
  `k8s-copilot config` prints the effective settings.
- **Safety hardening not in the original spec**: approval grants bound to the
  exact request; stale-state check before apply; Secret redaction for the
  model; terminal-escape sanitisation of all cluster/model text; replica cap;
  system-managed label/annotation keys refused; optional protected namespaces;
  https-only provider URLs; source-level guard tests.
- **Tests**: 97 top-level tests (150 including sub-tests) across all packages;
  `-race` clean on the concurrent packages; an opt-in live test (read-only +
  dry-run).
- **Docs**: README, `docs/architecture.md`, `docs/security.md`,
  `docs/decisions.md`, `docs/configuration.md`, `docs/testing.md`.

### Departures from the design document

See `docs/decisions.md` — K-01 (plain HTTP adapters instead of vendor SDKs),
K-02 (scale via merge patch on the scale subresource), K-03 (stale-state check
instead of resourceVersion preconditions), K-04 (partial discovery shown with a
warning), K-11 (an approved action has two audit entries — write-ahead — rather
than one).

### Reviewed

An independent adversarial review of the approval path found no way to apply an
unapproved change; the issues it did find were fixed before hand-over
(`docs/decisions.md` K-24).

### Not yet verified

The interactive smoke test with a real model and one approved change on a
cluster (tasks 8.3, 8.4) — see `docs/testing.md`.
