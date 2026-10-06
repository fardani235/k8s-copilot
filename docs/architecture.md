# Architecture

One Go binary. No server component, nothing installed in the cluster.

```
cmd/k2stui            wiring: config → cluster → audit → provider → agent → UI
internal/
  config              defaults < file < K2STUI_* env < flags
  kube                the ONLY package that talks to the Kubernetes API
    connect.go          kubeconfig resolution, clients, start-up probe
    types.go            discovery, type resolution (pods / po / Pod / pods.group)
    list.go             listing (server-side Table, generic fallback), get
    logs_events.go      pod logs, events
    describe.go         key-field summary, YAML
    write.go            MergePatch — the one write call in the program
    kubetest/           fake cluster for tests
  tools               the typed tool registry (everything the agent can do)
    registry.go         names, schemas, tiers, strict argument decoding
    read.go             list_resources, get_resource, describe_resource, get_logs, get_events
    mutate.go           scale, rollout_restart, set_labels, set_annotations → Plan
  approval            the human gate: Proposal, Gate.Ask, Grant
  audit               append-only hash-chained JSONL + verification
  agent               the loop: model ↔ tools, routing reads vs. gated mutations
  llm                 provider interface + Anthropic, OpenAI-compatible, stub
  tui                 Bubble Tea UI: browser, copilot pane, approval dialog
  textutil            sanitising text that came from the cluster or the model
  guard               tests that fail if a structural safety property breaks
```

Dependency direction (no cycles): `tui → agent → tools → kube`, with
`approval` and `audit` as leaves used by `tools`/`agent`/`tui`, and `llm` used
only by `agent` (and `config` for presets).

## The browser

- **Connection** (`kube.Connect`): standard kubeconfig loading rules with an
  optional explicit path/context, then one authenticated `/version` request so
  that "unreachable" and "credentials rejected" are reported at start-up, with
  the server address, instead of showing an empty cluster.
- **Discovery** (`Cluster.Types`): `ServerPreferredResources`, filtered to
  resources that support `list`, subresources dropped. Nothing is hard-coded,
  so CRDs appear like everything else. If discovery fails entirely, no type
  list is shown. If only some API groups fail (the classic stale
  `metrics.k8s.io`), the rest is shown **with a visible warning naming the
  missing groups**.
- **Listing** (`Cluster.List`): asks the API server for a `meta.k8s.io/v1`
  Table (`Accept: application/json;as=Table`). That is where kubectl gets its
  columns, so every type gets the columns its authors defined — READY /
  STATUS / RESTARTS for pods, a CRD's `additionalPrinterColumns` — with no
  per-kind code. If a server cannot produce a Table, it falls back to the
  dynamic client and generic NAME / STATUS / AGE columns. In all-namespaces
  mode a NAMESPACE column is prepended; cluster-scoped types ignore the
  namespace entirely.
- **Focus**: after every UI update the model writes what is on screen
  (context, namespace, type, selected resource, view) into a mutex-guarded
  `FocusStore`. The agent reads it when a request starts.

## The copilot

```
 you type a question
        │
        ▼
 agent.Run (own goroutine)                      UI goroutine
   user msg = [browser focus] + question
   loop (≤ max_iterations):
     provider.SendTurn(history, tools) ──► text / tool calls
     for each tool call (≤ max_tool_calls):
       unknown / malformed ─► error result, nothing runs
       read tool ───────────► registry.Read ─► result
       mutate tool ─────────► registry.Plan        (validate, read "before", build patch)
                              plan.DryRun          (server-side, dryRun=All)
                                 rejected ─► audit + error result; human never asked
                              gate.Ask(proposal) ════════► approval dialog
                                 (blocks; no timeout)  ◄════ approve / reject / edit
                                 reject ─► audit, "declined by user"
                                 edit ───► audit, re-plan with edited args, ask again
                                 approve ► audit "applying" ─► plan.Apply(grant) ─► audit result ─► real result
     tool results ─► history
   events ───────────────────────────────────────────────► transcript
```

- **The loop is ours** (`internal/agent`), not a provider SDK's. A provider
  implements one method, `SendTurn(ctx, Request) (Response, error)`: the
  conversation in, the model's next message out. It never runs a tool. That is
  what makes it possible to stop between "the model asked" and "it happened",
  and what makes the provider a configuration value.
- **Bounded**: `max_iterations` model calls and `max_tool_calls` tool calls per
  request. On hitting either, the request stops, the user is told, and every
  outstanding tool call gets a "not executed" result so the conversation stays
  valid.
- **Off the UI goroutine**: the loop talks to the UI only through an event
  channel and the approval gate's channel. The browser stays usable while the
  model thinks and while a proposal waits.

## The tools

A tool is a name, a JSON Schema, a tier, and a Go function over a typed
argument struct. Arguments are decoded strictly (unknown fields, wrong types
and trailing data are errors) and the model gets the error text back.

Read tools return text. Mutate tools **cannot execute**: the registry can only
turn a mutate call into a `tools.Plan` — the target, the exact patch body, the
before→after lines, reversibility, warnings — and a Plan can only be applied by
`Plan.Apply(ctx, grant)`, which refuses unless

1. the plan passed its server-side dry-run, and
2. `grant` is an `approval.Grant` whose digest matches this plan's request.

A `Grant` has unexported fields and no constructor; the only code that creates
one is the approve branch of `Gate.Ask`. So the type system, not a convention,
connects "a human said yes to this request" to "this request is sent".

Before sending, `Apply` re-reads the object and checks that the "before" values
the human was shown still hold; otherwise it refuses (`StaleError`) rather than
apply something the human did not approve.

All four verbs end in the same call, `kube.Cluster.MergePatch` — a JSON merge
patch on an existing object (or its `scale` subresource):

| Tool | Request |
|---|---|
| `scale` | `PATCH …/{deployments,statefulsets,replicasets}/NAME/scale` `{"spec":{"replicas":N}}` |
| `rollout_restart` | `PATCH …/{deployments,statefulsets,daemonsets}/NAME` `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"<time>"}}}}}` |
| `set_labels` | `PATCH …/NAME` `{"metadata":{"labels":{"k":"v","gone":null}}}` |
| `set_annotations` | `PATCH …/NAME` `{"metadata":{"annotations":{…}}}` |

The dry-run and the real request are byte-identical apart from `dryRun=All`.

## The audit trail

`internal/audit` appends JSON lines, each fsynced, to `audit.jsonl`: one per
proposal that ends without a change, and two for an approved one (the approval
before the request is sent, the result after). Each entry carries `seq`, `prev_hash` and `hash` (SHA-256 of the
entry's canonical encoding, which includes `prev_hash`). Verification checks
that every stored line is byte-for-byte the canonical encoding of what it
parses to, that it matches its hash, that it follows its predecessor, and that
the log has not been cut short relative to a small `audit.jsonl.head` anchor
(last seq + hash). Appends take an OS file lock, so two k2stui processes can
share a file without forking the chain.

Entry fields: time, session, proposal id, **intent** (your words), model,
model's reason, **tool**, **target** (context, server, apiVersion, kind,
namespace, name), **args**, before→after changes, the exact request, **dry-run
result**, **decision** (action, by whom, when), **outcome** (status, error or
detail).

## The UI

A single Bubble Tea model (`internal/tui`): a header, the browser pane, the
optional copilot pane (right rail), a footer of contextual key hints. Sub-views
(detail, logs, events, audit, help) form a back-stack over the listing. The
approval dialog replaces the whole screen while shown and owns the keyboard.

Every string that came from the cluster or the model passes through
`textutil.Sanitize` before it is drawn, so a log line cannot emit escape
sequences into your terminal.
