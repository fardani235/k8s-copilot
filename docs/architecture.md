# Architecture

One Go binary. No server component, nothing installed in the cluster.

```
cmd/k8s-copilot       wiring: config → cluster → audit → provider → agent → UI
internal/
  config              defaults < file < K8S_COPILOT_* env < flags
  kube                the ONLY package that talks to the Kubernetes API
    connect.go          kubeconfig resolution, clients, start-up probe
    types.go            discovery, type resolution (pods / po / Pod / pods.group)
    list.go             listing (server-side Table, generic fallback), get
    logs_events.go      pod logs, events
    describe.go         key-field summary, YAML
    metrics.go          the four reads behind the metrics screen, shared cache
    health.go           typed, capped reads behind the health screen, shared observation
    write.go            MergePatch — the one write call in the program
    kubetest/           fake cluster for tests
  metrics             the one model of "how busy is it": Snapshot → Listing (no I/O)
  health              the one model of "what is broken": Observation → Report (no I/O)
  tools               the typed tool registry (everything the agent can do)
    registry.go         names, schemas, tiers, strict argument decoding
    read.go             list_resources, get_resource, describe_resource, get_logs, get_events
    metrics.go          get_metrics
    health.go           get_findings
    mutate.go           scale, rollout_restart, set_labels, set_annotations → Plan
  approval            the human gate: Proposal, Gate.Ask, Grant
  audit               append-only hash-chained JSONL + verification
  agent               the loop: model ↔ tools, routing reads vs. gated mutations
  llm                 provider interface + Anthropic, OpenAI-compatible, stub
  tui                 Bubble Tea UI: browser, metrics screen, copilot pane, approval dialog
  textutil            sanitising text that came from the cluster or the model
  guard               tests that fail if a structural safety property breaks
```

Dependency direction (no cycles): `tui → agent → tools → kube`, with
`approval` and `audit` as leaves used by `tools`/`agent`/`tui`, `metrics` as a
leaf used by `kube`, `tools` and `tui`, `health` (which uses `metrics`) used
the same way, and `llm` used only by `agent` (and
`config` for presets).

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

## The metrics screen

`M` shows how loaded the cluster is right now: nodes, namespaces and pods at
the top, `enter` to drill from a node or a namespace into its pods and from a
pod into its containers. The copilot's `get_metrics` tool gives the model the
same thing. Both are thin renderers of one model, fed by one read:

```
 kube.Cluster.Metrics(namespace, maxAge)          internal/metrics (pure)
   four list requests, side by side:
     nodes.metrics.k8s.io   usage per node    ┐
     pods.metrics.k8s.io    usage per container├─► Build ─► Snapshot ─► List(Query) ─► Listing
     nodes                  allocatable        │              │                          │
     pods                   requests, limits   ┘              │            ┌─────────────┴─────────────┐
   each read has its own status (ok / absent / unavailable    │            ▼                           ▼
   / denied / unauthenticated / timed out / failed)           │      tui/metrics.go             tools/metrics.go
                                                              │      colour, bars, fitting      the same cells as text
   one cluster-wide read kept: reuse ≤ maxAge · one read      │      to the pane                 (get_metrics)
   for concurrent callers · hold-over ≤ 60s, marked ◄─────────┘
```

- **Source.** The resource metrics API, the one `kubectl top` reads, through
  the dynamic client: `list` only, no new dependency, nothing installed. If
  the cluster does not serve it, or refuses this user, that is what the screen
  says.
- **Usage against a bound.** Usage is joined with the nodes' allocatable and
  the pods' requests and limits, because "how loaded" is a ratio. The four
  reads fail independently, so each has its own status: the Nodes tab can say
  "permission denied" while the Pods tab works.
- **Unknown is not zero.** A quantity is a `metrics.Amount` with an `OK` flag;
  unknown is drawn as `—` and takes no part in sums or percentages. When the
  source a listing is made of is missing, the `Listing` has no rows and no
  columns at all — only the status — and the screen draws the reason instead
  of a table.
- **One model.** A `Listing` holds the formatted cells, percentages, each
  row's heat, the caveats, and the wording of every "unavailable" state. The
  screen and the tool both print it; neither computes anything. A `Query` is
  the same value whether it came from where the user drilled to or from the
  tool's arguments.
- **One read.** `Cluster.Metrics` keeps one cluster-wide read and cuts
  everything from it: everyone's node readings, and pod readings too — a
  question about one namespace is answered by narrowing it, not by a second
  read. Only if the cluster refuses the pods of the whole cluster are a
  namespace's pods read on their own, and those are joined to the same node
  readings. The tool accepts readings up to 15 s old (one metrics-server
  sample period), so while the screen is open the copilot is handed what is
  on the screen; if it has to read for itself, the screen takes up those
  readings when the tool call ends. Concurrent callers share one read, which
  runs detached with a 20 s bound. A `Snapshot` is immutable once built, which
  is what makes handing the same one to the UI goroutine and the agent's
  goroutine safe.
- **Refresh.** While the screen is visible, every `max(refresh_interval,
  10s)`, never on top of a read still out. The title shows the age of the
  *sample* and keeps it counting while the screen is up. If a refresh gets no
  usage after one that did, the earlier readings stay for at most 60 s, marked
  as not current.
- **Scope.** Cluster-wide. If pod metrics are refused cluster-wide, the screen
  falls back to the browser's namespace and says so.

## The health screen

`H` answers "is anything broken right now?" — and, as plainly, what was not
looked at. The copilot's `get_findings` tool gives the model the same report.
Both render one classification of one observation:

```
 kube.Cluster.HealthObservation(fallback, maxAge)        internal/health (pure)
   the four reads, side by side, typed, ≤ 500 rows each,
   each with its own status and a 10 s bound:
     pods                         ──────────────┐
     nodes                                      │
     deployments, statefulsets, daemonsets      ├─► Observation ─► Observe ─► Report
     events (type=Warning, last 15 min)         │                              │
   + the metrics cache (Cluster.Metrics, ≤15 s, │          ┌───────────────────┴──────────────┐
     no read of its own) ───────────────────────┘          ▼                                  ▼
                                                     tui/health.go                  tools/health.go
   cluster-wide; on 403 the fallback namespace,      findings by class, class       Report.Text, capped:
   saying so · cut at 500 rows, saying so ·          lines, age, filter, opens      findings cut, never
   one observation kept, reuse ≤ 15 s ◄──────────    detail / logs / events          a class's state
```

- **Typed reads.** `ListPods`, `ListNodes`, `ListDeployments`,
  `ListStatefulSets`, `ListDaemonSets` and `ListWarningEvents` return typed
  objects through the clientset — classification needs conditions, container
  states and restart counts — with the number of rows cut by the 500-row cap.
  `list` only; the guard tests are unchanged.
- **Attempt and classify.** Each read's error is classified exactly as the
  metrics screen classifies its own (denied, credentials rejected, not served,
  not answering, timed out, failed). A class is *watched*, *forbidden*,
  *unavailable* or *not applicable*; one some of whose reads answered is
  *watched in part* and names what it did not read. Nothing that was not read
  contributes a finding or an all-clear.
- **Namespace fallback.** Cluster-wide first; a read refused with 403 is
  retried for the browser's namespace, and the class says it covers only that
  namespace and why.
- **Truncation.** A read cut at 500 rows marks its class as watched in part,
  "only the first 500 pods were read (N more were not)". It is never clear.
- **Usage.** From the metrics cache, against `metrics.HeatOf`'s red (90 %) —
  the same rows the metrics screen draws red. No metrics API: the class is
  *not applicable*, not "nothing hot".
- **One observation.** The latest observation is kept with the fallback it
  was read with. Within 15 s it is handed out again — so the copilot is given
  what the screen shows, and the screen takes up what the copilot was given —
  and concurrent callers share one read. No state survives between
  observations: a condition that clears stops being listed.
- **Refresh.** While the screen is visible, every `max(refresh_interval,
  15s)`, never on top of a read still out. The title shows the age of the
  oldest read behind the report and keeps counting.

## The copilot

```
 you type a question
        │
        ▼
 agent.Run (own goroutine)                      UI goroutine
   user msg = [browser focus] + question
   system   = prompt (cached prefix)
            + [health snapshot]                  once per request, ≤ 10 s, ≤ 6 KB, after
                                                 the cache breakpoint; never in the history
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
- **Health snapshot in the request context.** Before its first model call,
  a request takes one snapshot of the cluster's classified findings and
  sends it as system context with every model call of that request
  (`llm.Request.TurnContext`: on Anthropic a second system block after the
  cache breakpoint, elsewhere the end of the system message, so the prompt
  and tools stay a cacheable prefix). Its source is
  `kube.Cluster.HealthObservation(ctx, fallback, kube.HealthFreshFor)`, with
  the screen's own fallback — the browser's namespace from the request's
  focus, else `kube.AnyFallback` — so it is the observation behind the health
  screen, reused when under 15 s old; an open health screen takes up a read
  the snapshot made. It is classified by `health.Observe` and printed with
  `Report.Text` under `agent.HealthSnapshotBytes` (6000): every class's state
  and the reading's age always, findings cut and counted to fit. The wait is
  bounded by `agent.HealthSnapshotTimeout` (10 s); on an error or timeout the
  block says "cluster health snapshot unavailable: <reason>" and tells the
  model to call `get_findings`, and the request goes ahead. The snapshot is
  not stored in the conversation, so history holds only what was said, and
  no old snapshot can be mistaken for a current one. A health question is
  therefore answered from context already sent, without a `list_resources`
  discovery pass.
- **Off the UI goroutine**: the loop talks to the UI only through an event
  channel and the approval gate's channel. The browser stays usable while the
  model thinks and while a proposal waits.

## The tools

A tool is a name, a JSON Schema, a tier, and a Go function over a typed
argument struct. Arguments are decoded strictly (unknown fields, wrong types
and trailing data are errors) and the model gets the error text back.

Read tools return text. (`get_metrics` returns the metrics screen's listing
as text — see above — and an error, not an empty table, when there are no
readings. `get_findings` returns the health screen's report — see above: the
summary, every class's state with what it covers and its thresholds, then the
findings, newest events first. It classifies nothing itself: it calls
`health.Observe` on the observation the screen holds, if that is under 15 s
old. Its optional `namespace` is used only as the fallback for a refused
cluster-wide read; left out, it is whatever the screen last used. To fit
`max_result_bytes` it drops findings, counting them per class, and never a
class's state. The seven read tools are `list_resources`, `get_resource`,
`describe_resource`, `get_logs`, `get_events`, `get_metrics`,
`get_findings`.) Mutate tools **cannot execute**: the registry can only
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
(last seq + hash). Appends take an OS file lock, so two k8s-copilot processes can
share a file without forking the chain.

Entry fields: time, session, proposal id, **intent** (your words), model,
model's reason, **tool**, **target** (context, server, apiVersion, kind,
namespace, name), **args**, before→after changes, the exact request, **dry-run
result**, **decision** (action, by whom, when), **outcome** (status, error or
detail).

## The UI

A single Bubble Tea model (`internal/tui`): a header, the browser pane, the
optional copilot pane (right rail), a footer of contextual key hints. Sub-views
(detail, logs, events, metrics, audit, help) form a back-stack over the
listing. The
approval dialog replaces the whole screen while shown and owns the keyboard.

Every string that came from the cluster or the model passes through
`textutil.Sanitize` before it is drawn, so a log line cannot emit escape
sequences into your terminal.
