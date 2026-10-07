## Context

See `proposal.md` — Why for motivation, and `specs/` for the behavior contract; this document covers how. Two constraints from the neighborhood shape the approach:

- `k8stui` (`/home/ridwan/works/k8stui`, Python/Textual) already established the cluster-access patterns this project reuses conceptually: connect from the active kubeconfig context, discover resource types generically, bounded log buffering, clean terminal restore. `k2stui` does not wrap or embed it.
- `volc-tui` (`/home/ridwan/workspace/volc-tui`) is a working Go/Bubbletea dashboard on this machine, so the TUI stack is already proven here.

The repository (`/home/ridwan/workspace/k2stui`) is a greenfield scaffold: OpenSpec only, no Go module yet.

## Goals / Non-Goals

**Goals:**

- One Go binary that both browses the cluster (generic discovery) and runs an agent against it.
- The agent's capabilities are a property of typed Go code, never of prompt text or shell strings.
- A mutating action cannot reach the cluster without passing server-side dry-run and then an explicit human decision.
- Model provider is a configuration detail behind an owned, pausable loop.
- The core safety and approval behavior is testable without a live cluster or a live model.

**Non-Goals:**

- Not replacing `k8stui`; not sharing code with it (different language).
- Not autonomous operation: every mutation is gated in this change.
- Not a general `kubectl` reimplementation: generic discovery covers browsing, but mutation stays a closed curated set.
- Not multi-cluster or in-app context switching in v1.

## Decisions

### D1 — Go + Bubbletea/Lipgloss/Bubbles, single binary

Same stack family as `volc-tui`, which is proven on this machine. Bubbletea's message/command model maps cleanly onto the approval gate (the proposal is a `tea.Msg`; the decision returns on a channel), and a single binary keeps distribution and kubeconfig handling simple. Alternatives: reuse the Python/Textual stack of `k8stui` (rejected — Go was chosen for the new layer), or a web UI (rejected — loses the terminal-first workflow).

### D2 — Generic resource browser over client-go discovery + dynamic client

The browser uses discovery to enumerate served resource types (including CRDs), the dynamic client to list and get them as unstructured objects, and a shared table renderer to produce kubectl-style columns (name, namespace, age, status/readiness) with per-type column hints derived from discovery when available. Typed access is reserved for operations where the API shape matters: logs, events, and mutation. Alternative: a curated switch over a handful of kinds (rejected by scope — generic discovery was chosen for v1).

### D3 — The loop is owned locally and provider-neutral

`k2stui` owns the agent loop and talks to the model through a small interface (`SendTurn` returning either tool calls or a final message). A concrete adapter per provider lives behind it. This is what makes "pause mid-tool-call" possible: the loop receives tool calls, routes read calls to immediate execution and mutate calls to the approval gate, and only then sends the tool results back. Alternatives considered: (a) delegate to an external agent over MCP — less code, but the agent experience moves out of the TUI and the gate becomes harder to enforce; (b) adopt the Anthropic SDK's stepped `toolrunner` directly — proven stepping support, but couples the loop to one provider. The interface keeps provider choice a config value; the Anthropic and OpenRouter shape both fit the same `SendTurn` contract.

### D4 — Two-tier typed tool registry; no shell strings

Tools are Go functions with typed argument structs and a schema, registered under a name. Read tools (`list_resources`, `get_resource`, `describe_resource`, `get_logs`, `get_events`) execute immediately. Mutate tools (`scale`, `rollout_restart`, `set_labels`, `set_annotations`) are gated. The model only ever emits a tool name plus args. This makes the agent's blast radius enumerable in code and directly testable with client-go fakes.

```
  model tool_use ──> registry.dispatch(name,args)
                          |
             read  ───────┴─────── mutate
               |                      |
        execute via client-go    dry-run via client-go
        return result            + emit approval request
                                       |
                                 block on channel
```

### D5 — Dry-run validates before the human sees the proposal

For a mutate tool the loop first issues the same write with `DryRun: ["All"]`. Server rejection becomes a tool result for the model and is recorded; the human never sees an invalid proposal. Acceptance produces the approval dialog content: verb, target, before→after, dry-run result, reversibility. Dry-run passing is necessary but not sufficient — apply can still fail (conflict, RBAC change, admission change), and that failure is handled as a tool result and audited.

### D6 — Approval is a blocking channel, no timeout

```
  agent goroutine ──proposal──> tea.Msg ──> TUI modal
        ^                                      |
        └────────── decision (channel) <───────┘
   approve -> execute -> real result
   reject  -> "declined by user" -> loop resumes
```

The goroutine blocks on a receive until the human answers; there is no default and no timeout that proceeds. Reject is a normal tool result, so the model can adjust and continue. This keeps all mutable state in the TUI and the loop free of shared-state races.

### D7 — Curated reversible mutation set (see specs)

`scale` uses the apps/v1 scale subresource; `rollout_restart` patches the pod template's `restartedAt` annotation; `set_labels`/`set_annotations` patch metadata. No delete, no arbitrary apply/patch, no image change — those are refused at the registry, which is the enforcement point, not the prompt.

### D8 — Append-only, tamper-evident local audit file

Each gated action appends one JSON line carrying the fields required by the `audit-trail` spec, chained with a hash of the previous entry so truncation or edits are detectable. The application only appends; it exposes no operation that rewrites or deletes entries. Alternative: ship entries to a backend service (rejected — adds an operational dependency to a local developer tool in v1).

### D9 — Focus injection and pane

The browser publishes a focus struct (namespace, resource type, selected resource) on every change; each turn's system/context includes the latest focus. The copilot is a toggleable side pane (right rail by default) so the browser stays primary. Alternative: a separate console layout (rejected — loses free situational context).

### D10 — Testing without cluster or model

- Cluster: `client-go` fake dynamic/typed clients, including dry-run rejection paths.
- Model: a scripted stub provider that emits predetermined tool calls, so the loop, gate, and audit can be driven deterministically.
- Errors: fake clients return Forbidden/Conflict to assert surfacing behavior.

## Risks / Trade-offs

- **Provider-neutral loop is more work than adopting one SDK's runner** → keep the interface tiny (`SendTurn` in, tool calls or message out), back it with a stub first, and add one real adapter plus another to prove neutrality.
- **Generic columns for arbitrary kinds can render poorly** → derive columns from discovery where possible and fall back to a safe default set; treat column quality as cosmetic, never as a correctness requirement.
- **Dry-run success does not guarantee apply success (TOCTOU)** → handle apply failure as a first-class audited outcome; consider resourceVersion preconditions on patches where the API supports them.
- **Prompt injection from cluster content** (a log line or event that instructs the model) → mutation is gated by code and the human regardless of model output; read tools are limited to reads; the approval dialog shows the exact target and args so the human sees what is being proposed.
- **Agent cost/latency runaway** → bound iterations and tool calls per turn (spec: bounded turns), surface the limit, and keep prompt caching as an adapter capability.
- **Blocking the loop on approval** → the loop runs off the UI goroutine and only communicates via messages and a channel; the browser stays responsive while a proposal waits.
- **Tamper evidence is detection, not prevention** → v1 provides detection via the hash chain; stronger guarantees (signing, remote sink) are out of scope.
- **One binary with cluster credentials and a model key** → v1 uses the caller's kubeconfig and env-supplied key; a scoped ServiceAccount and key handling are deferred to a later change.

## Migration Plan

Greenfield: create the Go module and build from scratch; no data or existing code migrates. Rollback is deleting the binary; `k8stui` and `volc-tui` are unaffected.

## Open Questions

- Default copilot pane placement (right rail vs. bottom) — cosmetic, changeable without affecting specs.
- Whether to ship one real provider adapter or two in this change — architecture is fixed either way; the stub plus one adapter is the floor.
- Default listing refresh interval — a tunable default, not a behavior contract.
