## Why

k8s-copilot shows what exists and what is misbehaving, but not how busy
anything is. CPU and memory are the first things an operator looks at, and
today they are invisible — to the person at the keyboard and to the copilot,
which has to infer "it is slow" or "it ran out of memory" from logs and events
alone.

Most clusters expose current usage through the resource metrics API
(`metrics.k8s.io`, the source of `kubectl top`). Not all do, and where it
exists it is sometimes broken or not something the user may read. The tool's
defining property is that it is honest about failure: it says "you are not
allowed" rather than showing an empty list. Load has to be shown under the
same rule, where the temptation to draw a missing reading as `0%` is strongest
and the cost of doing so is highest.

## What Changes

- Add a **metrics screen**, opened with `M` from any browser view: current CPU
  and memory per node, per namespace and per pod, with drill-down from the
  cluster to one container (node → its pods → a pod's containers; namespace →
  its pods → containers). Sortable, filterable, keyboard-driven, no set-up.
- Show usage **against what bounds it**: a node's allocatable, a pod's and a
  container's requests and limits. Readings near a hard bound (allocatable,
  limit) are highlighted.
- The screen **refreshes itself** while visible and states how old its sample
  is. A refresh that fails keeps the last readings on show for a short while,
  visibly marked as not current.
- Add a copilot **read tool, `get_metrics`**, with the same levels (nodes,
  namespaces, pods, containers). The system prompt tells the model to check
  load with it rather than infer it, and what a missing source means.
- Screen and tool are two renderings of **one model** (`internal/metrics`) fed
  by **one read** (`kube.Cluster.Metrics`), so they cannot show different
  numbers, percentages, "hot" classifications or explanations.
- **Missing sources are states, not zeros.** The metrics API being absent
  (404), registered but not answering (503), refused (403), not accepting the
  credentials (401), or too slow is each reported in its own words, on the
  screen and to the model. A missing
  reading is drawn as `—`, never `0`. A source that is missing produces no
  table at all.
- A user who may read pod metrics only in their own namespace still gets a
  screen: it falls back to the browser's namespace and says what it is not
  showing.
- From a row, `l`, `e` and `d` open the existing logs, events and detail views
  for that pod / node / namespace, and return to the same place.
- **Read-only.** Four `list` requests through the existing dynamic client. No
  new dependency, no new permission, nothing installed in the cluster, no new
  configuration.

Not in this change: history, trends, graphs, alerts; usage columns in the
ordinary resource listings; any source other than `metrics.k8s.io`.

## Capabilities

### New Capabilities

- `cluster-metrics`: read current CPU and memory usage from the cluster's
  metrics API, join it with capacity, requests and limits, present it as a
  navigable node / namespace / pod / container hierarchy, and state plainly
  when the source is absent, failing, refused or stale.

### Modified Capabilities

- `agent-copilot`: gains a requirement that the copilot can read the same
  usage readings the metrics screen shows, and is told when they are
  unavailable rather than given zeros.
- `resource-browser`: the focus exposed to the copilot covers the metrics
  screen (what is listed and which row is selected); the keyboard layout
  gains the metrics screen.

## Impact

- **New package** `internal/metrics` (pure, no I/O): `Snapshot`, `Build`,
  `Query` → `Listing`, formatting, the wording of every "unavailable" state.
- `internal/kube/metrics.go`: the four reads, error classification, a small
  shared cache (reuse, in-flight sharing, hold-over). `connect.go`: one field.
  `kubetest`: the fake cluster can serve, fail or omit the metrics API.
- `internal/tools/metrics.go`, `registry.go`: the `get_metrics` read tool (the
  registry is now six read tools and four mutate tools).
- `internal/agent/agent.go`: two system-prompt paragraphs.
- `internal/tui/metrics.go` (new), `model.go`, `browser.go`, `views.go`: the
  screen, the `M` key, the tick, focus, footer hints, help, the shared filter
  input, opening logs on a chosen container.
- `internal/tui/copilot.go`: a pre-existing overflow of the "copilot is not
  available" text in a short pane, found while testing small terminals, is
  fixed.
- `Makefile`: `race` now covers `internal/kube` and `internal/metrics`.
- Docs: README, `docs/architecture.md`, `docs/decisions.md` (K-25 – K-38),
  `docs/security.md`, `docs/testing.md`, `docs/configuration.md`, CHANGELOG.
- **No new dependencies.** `go.mod` is unchanged.
- **Security posture unchanged**: `internal/guard` still passes untouched —
  one write call in the program, one raw HTTP request (a GET), no subprocess.
  What is new for the model provider: pod, namespace and node *names* and
  their usage figures are sent when the copilot calls `get_metrics`.
