## Why

`k8stui` proved the read-and-exec terminal workflow, but it only *shows* a cluster — it does not help decide what to do next. Troubleshooting still means reading logs and events by hand, forming a hypothesis, and then typing the fix yourself. At the same time, giving an LLM a mutating cluster client is the one genuinely dangerous part of an "agentic" tool. This change introduces `k2stui`: a Go/Bubbletea copilot that investigates with the model, proposes cluster changes, and only acts after a human approves a server-validated plan. `k8stui` stays as-is; `k2stui` is a separate layer beside it.

## What Changes

- Introduce a new Go application (single binary, Bubbletea/Lipgloss/Bubbles) that connects to the **active kubeconfig context** and browses the cluster.
- Add a **generic resource browser** built on client-go discovery + dynamic client: list namespaces, discover any served resource type, list resources with kubectl-style columns, and show detail + YAML. Listing scope covers v1 read needs (list/get/describe/logs/events). This is generic discovery from day one, not a curated kind list.
- Add an **agent copilot pane** beside the browser (browser primary, copilot as a toggleable sidecar). The panel runs a pausable agent loop that receives the browser's current focus (namespace, selected kind, selected object) as context each turn.
- Define a **typed tool registry** over client-go. The agent never constructs `kubectl` strings; it names a tool and passes structured arguments. Tools are split into two tiers: **read** (auto-executed to gather context) and **mutate** (always gated).
- Gate mutations with a **mandatory human approval flow**: the proposed change is validated with a server-side dry-run before it is shown; the human sees verb, target, before→after, dry-run result, and reversibility; the agent loop blocks until the human approves, edits, or rejects.
- Restrict v1 mutations to a **closed set of curated, reversible verbs** — `scale`, `rollout_restart`, `set_labels`, `set_annotations`. No delete, no arbitrary apply/patch, no image changes.
- Add an **append-only audit trail** of every gated action (intent, tool, args, dry-run result, approver, outcome).
- Make the model provider **provider-neutral from day one**: the agent loop is owned by `k2stui` and talks to a thin provider interface, so model choice is configuration.

## Capabilities

### New Capabilities
- `resource-browser`: connect to the active kubeconfig context, discover served resource types generically, list and navigate resources with kubectl-style columns, render detail/YAML and logs, and expose the current focus state that the copilot consumes.
- `agent-copilot`: run a pausable, provider-neutral agent loop over a typed read/mutate tool registry, inject browser focus as context, and gate every mutating action behind a server-side dry-run and explicit human approval.
- `audit-trail`: append-only, durable record of each gated action and its outcome, sufficient to reconstruct what the agent attempted, what the human approved, and what changed.

### Modified Capabilities
<!-- None - this is the first change in this project; openspec/specs/ is empty. -->

## Impact

- **New code**: a new Go module at the repository root (`cmd/`, `internal/`); no existing code is modified. `k8stui` and `volc-tui` are untouched and remain separate.
- **New dependencies**: `client-go` (dynamic client, discovery, typed scale subresource, logs, events, server-side dry-run); `bubbletea`/`lipgloss`/`bubbles` (TUI); a provider-neutral model interface with at least one concrete provider SDK behind it; standard Go testing plus `client-go` fake clients and a stub provider for deterministic loop tests.
- **Runtime prerequisites**: a valid kubeconfig with at least one context and reachability to its API server, plus a model provider API key supplied via environment. Cluster access resolves by standard kubeconfig rules (`KUBECONFIG`, then `~/.kube/config`) with an optional `--context` override.
- **Security**: the tool acts with the caller's kubeconfig credentials and requests no additional permissions in v1; approval is the gate. A dedicated scoped ServiceAccount is deliberately deferred to a later change.
- **Out of scope for v1**: delete, arbitrary apply/patch, image changes, cordon/drain, in-app context switching, and autonomous (no-human) operation.
