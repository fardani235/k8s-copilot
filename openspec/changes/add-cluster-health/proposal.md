# Proposal

## Why

k8s-copilot can list, describe, read logs and events, and — since the metrics
screen — show how loaded the cluster is. But it cannot answer the first
question an operator actually has when they open it: **is anything broken right
now?** That answer exists only as a reading exercise across pods, nodes,
workload status and events, repeated per resource type and namespace, and it is
easy to mistake "I did not see a problem" for "there is no problem" when a read
was refused.

The metrics screen established the pattern that makes a truthful answer
possible: classify what is wrong, say which sources are unavailable, and never
let an absent source look like an all-clear. This change applies that pattern
to workload health.

## What Changes

- Add a **cluster health capability**: classify the current state of the cluster
  into findings, cluster-wide — crash-looping pods, image-pull failures,
  pods not ready, nodes NotReady or under memory/disk pressure,
  Deployments/StatefulSets/DaemonSets that are not progressing, and recent
  Warning events.
- Report **per-class availability**. A class the caller may not read, or whose
  source is absent, is named as not watched and produces no findings — it is
  never drawn as healthy.
- Add a **health screen** reachable from the browser, listing the findings and
  saying plainly what is not being watched. It refreshes while visible and
  shows the age of its reading.
- Add a **`get_findings` read tool**, so the copilot can answer "what is broken?"
  from the same classified findings the screen shows, rather than inferring
  health from listings.
- The health screen becomes part of the **focus** reported to the copilot, like
  the metrics screen.
- No new configuration; no cluster-side install; no new credentials.

Not in this change: notifications or any background process; trends or history;
metrics thresholds beyond what the existing heat model already classifies;
any change to the mutation set, the approval gate or the audit trail.

## Capabilities

### New Capabilities

- `cluster-health`: classify current cluster conditions cluster-wide into
  findings, state for each class whether it is watched, forbidden, unavailable
  or not applicable, present those findings on a screen and to the copilot, and
  never present an unavailable source as an absence of problems.

### Modified Capabilities

- `agent-copilot`: the copilot gains a read tool that returns the cluster's
  current findings, and is told that an unwatched class means "not checked",
  not "healthy".
- `resource-browser`: a keyboard route to the health screen, its refresh and
  navigation behavior, and its inclusion in the focus exposed to the copilot.

## Impact

- **New package** `internal/health` (pure, no I/O): the finding and class
  model, classification from typed list results, per-class availability, and
  the wording of every "not watched" state. Thresholds reuse the existing
  metrics heat model so a "hot" finding means what the metrics screen means.
- **`internal/kube`**: read-only typed listing helpers the monitor needs —
  pods, nodes and the three rollout workloads, cluster-wide with a namespace
  fallback when the cluster-wide list is refused; Warning events; and the
  existing `Cluster.Metrics` snapshot. No new write path, and the existing
  metrics cache is reused rather than duplicated.
- **`internal/tui`**: a new view, its key and footer hint, the shared filter
  and refresh tick, its focus text, and help. It opens the existing detail,
  logs and events views for a selected finding.
- **`internal/tools`, `internal/agent`**: the `get_findings` read tool and the
  system-prompt paragraph that says what an unwatched class means.
- **Unchanged on purpose**: `internal/guard` still passes without amendment —
  one cluster write call, one raw HTTP request (a GET), one approval call site,
  no subprocess. `go.mod` untouched. No new configuration.
- **Docs**: README, `docs/architecture.md`, `docs/decisions.md` (K-39…),
  `docs/security.md` (the read-only posture is unchanged and should say so),
  `docs/testing.md`, CHANGELOG.
