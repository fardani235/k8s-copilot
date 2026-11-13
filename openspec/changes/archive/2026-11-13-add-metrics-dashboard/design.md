## Context

k8s-copilot is a read-only-by-default terminal browser with a copilot beside
it. Three things about the existing code shape this design:

- **`internal/guard` reads the source tree** and fails the tests if
  `internal/kube` gains a write-verb call, a second raw HTTP request, or a
  subprocess import. Whatever reads metrics has to live inside those rules.
- **`kube.Summarize` is already rendered twice** — by the detail view and by
  the `describe_resource` tool. That is the existing precedent for keeping the
  screen and the copilot consistent, and it is the idea this change takes
  further.
- **Failure is a first-class state everywhere**: a forbidden listing is drawn
  as "Permission denied… not an empty list", partial discovery carries a
  visible warning (K-04). Metrics must not be the one place where a missing
  value quietly becomes a `0`.

The request: a screen that shows, at a glance and live, how loaded the cluster
is per node, namespace and pod, with drill-down to a container; the copilot
able to read the same numbers; read-only; plain about a missing or refusing
source; degrading gracefully.

## Goals / Non-Goals

**Goals**

- An accurate picture of *right now*, from the cluster to one container.
- "Unknown" is never shown as zero or as an empty table — on the screen or to
  the model.
- The screen and the copilot cannot contradict each other, by construction
  rather than by care.
- Nothing to install, configure or grant. Only `list` requests.
- Stays usable on a small terminal, with a slow or flaky source, and for a
  user with partial permissions.

**Non-Goals**

- History, trends, graphs, alerts, thresholds the user configures.
- Sources other than `metrics.k8s.io` (Prometheus, the kubelet).
- Usage columns inside the ordinary resource listings (argued for below as a
  follow-up, not done here).
- Any change to the mutate tools, the approval gate or the audit trail.

## Decisions

### D1 — The numbers come from `metrics.k8s.io`, through the dynamic client

Two `list` calls — `nodes.metrics.k8s.io` and `pods.metrics.k8s.io` — give
usage for every node and every container. They go through the dynamic client
that `internal/kube` already holds.

*Why this source.* It is the one standard, vendor-neutral place a cluster
publishes current usage, it is what `kubectl top` reads so operators already
trust and recognise the numbers, and it needs nothing but the `list` verb.

*Why the dynamic client.* The typed `k8s.io/metrics` client would add a
dependency to parse two small, stable shapes. The dynamic client is already
there, already covered by the guard tests, and the fake cluster can serve the
group with the same machinery it uses for custom resources. `go.mod` does not
change.

*Alternatives rejected.*

| Alternative | Why not |
|---|---|
| Kubelet Summary API via `nodes/{n}/proxy/stats/summary` | Needs `nodes/proxy`, a far stronger permission than most users have (and one that should not be asked for); one request per node; breaks "no new permissions" in spirit. |
| Prometheus | Not standard: has to be discovered or configured, and authenticated separately. Violates "nothing to set up". Would be the right source for history, which is out of scope. |
| Typed `k8s.io/metrics` client | A dependency for nothing (above). |
| Ask first with `SelfSubjectAccessReview` | That is a `create` — it would be a second write-shaped call in a program whose safety argument is "there is exactly one". And the answer would be a prediction; trying the read and classifying the failure is the truth. |
| Consult discovery to decide availability | Discovery is cached and is exactly what goes stale when metrics-server breaks (K-04). The call itself is authoritative. |

The API version is fixed at `v1beta1`, the only version the metrics API has
ever served.

### D2 — Usage is always shown against a bound, so there are four reads, not two

"How loaded" is a ratio. `1.9 cores` means nothing until it is set against the
node's 2 allocatable; `490Mi` matters because the limit is `512Mi`. So usage is
joined with two more `list` calls the browser makes anyway: `nodes` (for
allocatable) and `pods` (for requests, limits, node, phase, restart counts).

This is also what makes the copilot half of the feature possible at all. "Is
it CPU-saturated or out of memory?" cannot be answered from usage alone: it is
a question about a limit.

Each of the four reads carries **its own status**. They fail independently in
practice — a namespace-restricted user can usually list pods but not nodes; a
broken metrics-server takes both metrics reads down and leaves the core ones
up — and the screen has to be able to say which.

The two core reads ask for `resourceVersion=0`, letting the API server answer
from its cache: allocatable, requests and limits change rarely, usage is a
sample that is seconds old regardless, and a quorum read of every pod on each
refresh is a cost a monitoring view should not impose. The metrics reads do not
(the metrics API has no such cache).

Conventions, chosen to match what the numbers mean while a pod runs:

- Finished pods (`Succeeded`, `Failed`) are left out: they use and reserve
  nothing. Pending pods are listed, with no usage.
- The containers listed are the ones that run while the pod is up: regular
  containers and restartable init containers (sidecars, labelled). An
  ordinary init container or an ephemeral one is listed **only when the
  metrics API reports it running** — and then with the bounds its own spec
  declares. While an init container runs, the pod is measured against *that*
  container's bounds, not against containers that have not started.
- A container the metrics API reports but the spec does not describe has
  unknown bounds (`—`), never "none"; and then so does its pod.
- A pod's limit is known only if *every* running container has one. One
  unlimited container makes the pod unlimited; showing the sum of the others
  as "the limit" would understate what it may use.
- **Limits bind containers, not pods.** A pod row is as hot as its hottest
  container and says which (`app: memory 99% of limit`), even when the
  pod-level share looks comfortable because a neighbour has room.
- CPU is carried in **nanocores**, the unit the metrics API reports in, and
  rounded only for display (up to the next millicore, as `kubectl top` does).
  Rounding each container up before adding turns 300 idle containers at a
  tenth of a millicore into 300m instead of 30m.
- Cluster totals come from node metrics, not from summing pods, so they
  include the kubelet and system daemons. A node with no sample is left out
  of *both* sides of the ratio and the summary says so; if a reporting node's
  allocatable is unknown there is no cluster percentage at all.
- A sample that lists no containers measured nothing: the pod has no reading.
- Quantities that cannot be readings (negative, more than a billion cores or
  an exabyte) are unknown rather than allowed to overflow.

### D3 — Unknown is a value of its own

Every quantity is an `Amount{V int64; OK bool}`. The zero value is *unknown*,
deliberately: a forgotten assignment shows as `—`, not as a believable `0`.
Sums add the known parts and are unknown only if nothing was known; where that
makes a total a lower bound, the listing says so.

Three things that look alike are kept apart:

| Situation | Shown as |
|---|---|
| The metrics API returned no sample for this row | `—`, plus a note saying how many and why (not running / not reported yet / node NotReady) |
| No limit or request is set | `no lim`, `no req`, `none` |
| The pod spec could not be read, so the bound is unknown | `—`, plus a note saying requests and limits are unknown |
| Measured zero | `0m`, `0` |

And one rule above the cells: **when the source a listing is made of is not
available, there is no table.** `Listing.Unavailable` is set, there are no
rows and no columns, and the screen draws the reason instead. A table of
dashes under a banner was considered and rejected — at a glance it still looks
like a table of data.

### D4 — Six ways to be missing, classified by trying

| State | What it is | How it is recognised |
|---|---|---|
| Absent | no metrics-server: the group is not served | 404 |
| Unavailable | APIService registered, backend not answering | 503, 500, unexpected server error |
| Denied | this user may not read it | 403 |
| Unauthenticated | the credentials were not accepted (expired token) | 401 |
| Timed out | no answer in time | context deadline, 504, server timeout, network timeout |
| Failed | anything else, including being rate-limited (429) | — |

Each has its own headline and explanation, written once in
`internal/metrics` and used verbatim by the screen and by the tool's error
result. They are different because the operator's next move is different:
install something, wait or fix something, ask for access, log in again, or
retry. (401 and 403 were one state at first; the review pointed out that an
expired token was being explained as a missing permission, with the advice to
try a narrower read.)

### D5 — One model, two renderers; one read, shared

This is the answer to "the copilot and the screen should never contradict each
other", and it has two halves.

**Same computation.** `internal/metrics` is a pure package with no I/O:

```
four raw lists ──Build──▶ Snapshot ──List(Query)──▶ Listing
                                                     ├─ Columns, Rows (cells as text)
                                                     ├─ Summary (gauges, facts)
                                                     ├─ Unavailable / Empty
                                                     └─ Notes (caveats)
```

A `Listing` already contains the formatted cells, the percentages, each row's
heat and flags, the caveats, and — when there are no readings — the status
whose `Headline()` and `Explain()` say why. The screen adds colour, bars and
column-fitting; the tool prints the same cells as aligned text. **Neither
computes a number.** A `Query` is the same value whether it was built from
where the user drilled to or from the tool's arguments, so the tool's levels
are exactly the screen's levels.

**Same readings.** Rendering identically does not help if the two are looking
at samples taken ten seconds apart. `kube.Cluster.Metrics(ctx, namespace,
maxAge)` therefore keeps **one cluster-wide read**, and everything is cut from
it:

- everyone's node readings come from it;
- pod readings come from it too — a question about one namespace is answered
  by *narrowing* it, never by a second read;
- only if the cluster refuses this user the pods of the whole cluster are a
  namespace's pods read on their own, and they are then joined to *the same*
  node readings (`Snapshot.WithPodsOf`);
- readings no older than `maxAge` are handed out as they are — the tool passes
  15 s (one metrics-server sample period), so while the screen is open the
  copilot is given the readings on the screen;
- concurrent callers share one read, which runs detached with its own 20 s
  bound, so a caller giving up neither cancels it for the others nor poisons
  the cache.

When the copilot has to read the cluster itself — what the screen holds is
older than 15 s, which happens with refresh off or a long interval — the
screen **takes up those readings** as soon as the tool call ends, from the
cache and without another read. So the model is never quoting numbers newer
than the ones on show.

The first version kept a separate snapshot per namespace beside the
cluster-wide one. The review showed three ways those could disagree (a
namespace entry outliving a newer cluster-wide read; held readings on the
screen but "unavailable" from the tool; a namespace failure shadowing a later
success). One read that everything is cut from removes the class of bug
rather than the three instances.

Each result also carries its sample time, on the screen and in the tool
output, so that the one remaining way to differ — the screen refreshing while
the model is still composing its answer — is visible as a difference in age
rather than an unexplained contradiction.

*Alternatives rejected.* Two formatters over a shared struct (the
`Summarize` approach as it stands): the divergence risk is exactly in the
formatting — rounding, units, which denominator. Having the tool return JSON
for the model to format: moves the arithmetic into the model. Having the tool
read what the TUI last drew: couples the agent to the UI and fails when the
screen is closed.

### D6 — The hierarchy is three flat listings and a drill-down path

```
 metrics · cluster › node node-a › pod shop/web-1          sampled 8s ago · 15s window
 node node-a  ███░░░░░░░ CPU 1.2 of 4 allocatable (30%)   ████████░░ memory 6Gi of 8Gi allocatable (75%)
 Ready · 2 pods using CPU 490m, memory 518Mi
 pods on node node-a · 2  by cpu ↓
 NAMESPACE  POD      CPU  %CPU/R  %CPU/L    MEM  %MEM/R  %MEM/L
 shop       web-1   490m    163%  no lim  510Mi    177%  no lim
```

- **Top level: three tabs** — nodes, namespaces, pods (`1` `2` `3`, `←` `→`).
- **`enter` drills in**: a node or a namespace → its pods → a pod's
  containers. **`esc` goes back up one level**, landing on the row it came
  from, and from the top leaves the screen.
- A **breadcrumb** says where this is; a **summary line** shows what the rows
  add up to (the cluster, the node, the namespace, the pod) as gauges.
- Rows are **sorted by CPU, busiest first** (`s` cycles memory, name), and the
  cursor follows the *row*, not the position, so a refresh that reorders the
  list does not move the selection.
- `/` filters by name; `l` `e` `d` open the existing logs, events and detail
  views for the row.

*Why not a tree.* There are two hierarchies over the same pods — physical
(node → pod) and logical (namespace → pod). A tree has to pick one, and a
tree with a few thousand leaves is unreadable in a terminal. Tabs plus a path
serve both, and every listing is a flat table that sorts.

*Why not stacked panels* (nodes over namespaces over pods, all visible). It
is the classic "dashboard" look, and it fails the constraint that matters:
at 80×24 each panel gets four rows, and beside the copilot nothing fits.

Percentages are shown against requests (`%…/R`) and limits (`%…/L`) for pods,
against allocatable for nodes. Rows are tinted amber at 75 % and red at 90 % of
a **hard** bound only (allocatable, limit). Usage above a *request* is normal
for a burstable pod and is not flagged. The thresholds live in
`internal/metrics`, so the screen's red row is the tool's `HIGH`.

### D7 — Cluster-wide first; fall back to the browser's namespace when refused

The screen opens on the whole cluster regardless of the browser's namespace:
the brief is "how loaded the cluster is", and the hierarchy *is* the scoping
mechanism (the Namespaces tab, then `enter`).

If pod metrics are refused cluster-wide and the browser is in a namespace, the
screen reads that namespace instead and says so: *"Permission denied reading
pod metrics across all namespaces: showing namespace shop only."* The Nodes
tab then shows its own refusal. Every refresh asks for the whole cluster
again, so the screen widens by itself if the refusal is lifted. If the browser
is on "all namespaces" there is nothing to fall back to, and the unavailable
state says how to get one.

In a namespace-scoped snapshot, per-node pod counts are not shown: a count of
one namespace's pods presented beside a node would read as the node's total.

### D8 — Live means "as fresh as the source", and says how fresh

- The screen re-reads every `max(refresh_interval, 10s)` — only while it is
  visible, and never while a read is still out. metrics-server samples every
  15 s by default; polling at the listing's 5 s would return the same numbers
  three times and triple the load. `refresh_interval: off` turns this off too,
  and the title says so.
- The title shows the age of the *sample* (the metrics API's own timestamp),
  not the time of the request, and flags it past 90 s. Nodes and pods are
  sampled separately; the age is that of the older of the two, so one fresh
  source cannot vouch for a stale one.
- The age **keeps counting on a screen nobody touches**: a redraw timer runs
  while the screen is the current view (5 s granularity). Without it the
  title would say "sampled 8s ago" for as long as the refresh interval — or
  for ever with refresh off — and old readings would go on claiming to be
  fresh.
- **Hold-over.** If a refresh gets no usage at all after an earlier one did,
  the earlier readings stay on show for up to 60 s, with the title reading
  "refresh failing since… — showing the readings from… ago" and a note giving
  the cause. After that the unavailable state replaces them. One dropped poll
  from a flaky metrics-server should not flip the screen to an error and back;
  stale numbers should not sit there indefinitely behind a warning either.
  The hold-over is implemented in the shared cache, so the copilot gets the
  same marked snapshot (and its result starts with a WARNING).

### D9 — `M` opens it, from any browser view

Capital letters open full screens (`A` is the audit trail). `m` is taken
(maximize the copilot); `u` would shadow the viewport's half-page-up in the
detail view. A slip between `m` and `M` costs one `esc`.

### D10 — It degrades by giving things up in a fixed order

The browser pane can be as narrow as 24 cells (60 columns with the copilot
open) and as short as 12 lines.

- **Columns** carry a drop order. Bars go first, then the node column, then
  the request percentages, then absolute values where a percentage remains;
  the name column gives up its excess, then most of itself. Name, CPU and MEM
  never go.
- **Gauges** have a ladder: full text with bars → stacked on separate lines
  if there is height → abbreviated (`CPU 3.1/6 alloc (52%)`) → shares only
  (`CPU 52%`).
- **Freshness** shortens to `8s old`, and when there is not room for it and
  the breadcrumb, the breadcrumb goes: how old the numbers are matters more.
- The renderer **guarantees its own bounds** (`fitBox`): whatever a part
  draws, the screen never hands the frame more lines or wider lines than the
  pane has.

### D11 — No new configuration

Nothing to set up was a requirement, and nothing here needs a knob. The
existing `refresh_interval` is honoured (floor 10 s for this screen).

## Where this departs from the brief, and why

**1. Usage against requests and limits, not usage alone.** The brief asks for
CPU and memory per node, namespace and pod. Delivered literally that is
`kubectl top` in a pane — and it would leave the copilot unable to answer its
own headline question. Every level therefore shows usage against its bound,
which costs two extra `list` calls of things the tool already reads.

**2. Not a dashboard; a view in the browser.** "Dashboard" suggests a separate
place with its own widgets. This is one more view in the browser's back-stack:
same pane, same `esc`, same filter key, and `l`/`e`/`d` jump from a hot pod to
its logs, events and detail. The copilot's focus includes it, so "why is this
one so busy?" works with a node selected.

**3. Per-source honesty rather than one on/off.** "When the metrics source
isn't there… say that plainly" reads as a single state. In practice there are
four sources that fail independently, and treating them as one would either
hide working data (pod metrics, for the user who cannot list nodes) or show
percentages that cannot be computed.

**4. "Live" is bounded by the source.** The screen does refresh itself, but it
does not pretend to be fresher than a 15-second sampler: it shows the sample's
age instead of refreshing faster.

**5. A better shape still, not done here: usage in the ordinary listings.**
The strongest version of "the first thing an operator looks at" is CPU and MEM
columns directly in the pods and nodes listings, so that no screen change is
needed at all. It is deliberately left out of this change: those listings take
their columns from the API server's Table printer (K-16), so it means merging
a second source into a server-defined table, and it multiplies the honesty
states into every listing. With `internal/metrics` in place it is a contained
follow-up, and worth doing.

## Risks / Trade-offs

- [Clock skew] The sample's age compares the cluster's timestamp with the
  local clock. A laptop two minutes ahead will see every sample as two minutes
  old and flagged. → Negative ages are clamped to zero; the limit is
  documented. The API offers no server time to correct with.
- [Large clusters] Every refresh lists every pod. → Served from the API
  server's cache, only while the screen is visible, at most every 10 s, capped
  at 20 000 items per source with a visible "truncated" note. A cluster large
  enough for this to matter should be looked at through Prometheus.
- [The model answers from an older snapshot than the screen now shows] The
  screen refreshes while the model is still thinking. → Both state their
  sample age. Not eliminable without freezing the screen during a turn.
- [A local clock *behind* the cluster's] makes an old sample look new
  ("sampled 0s ago"), the mirror image of the skew above. Not detectable from
  the API.
- [Caveats in a very small pane] Notes under the table get a quarter of the
  pane plus whatever the rows leave. With many rows in a 12-line pane a long
  caveat is cut, with a visible `…`. The title, the `—` cells and the colour
  are not affected.
- [Init containers] The handling of a *running* init container assumes the
  metrics API reports it under its own name, as metrics-server does. It was
  tested with constructed samples, not watched on a live cluster.
- [Memory is "working set"] The metrics API reports working-set memory, which
  is what the OOM killer acts on but not what `ps` shows. → Stated in the docs.
- [Not modelled] Pod-level `resources`, pod overhead, ResourceQuota,
  ephemeral-storage, GPUs. → Out of scope; nothing is shown rather than
  something approximate.
- [Hold-over shows old numbers] By design, for at most 60 s, never unmarked.
- [Metric names sent to the model provider] `get_metrics` sends pod, namespace
  and node names with their usage. → No different in kind from
  `list_resources`; noted in docs/security.md.

## Migration Plan

None. No stored state, no configuration, no API change. On a cluster without
the metrics API the only visible difference is a screen that says so. Rollback
is reverting the commit.

## Open Questions

- Should the Namespaces tab show ResourceQuota as the namespace's bound where
  one exists? It is the natural denominator and one more `list`; left out to
  keep this change to four reads.
- Usage columns in the pods and nodes listings (departure 5): do it next?
