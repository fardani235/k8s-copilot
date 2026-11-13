# Decision log

Decisions made while implementing `openspec/changes/archive/2026-10-07-add-k2stui`. The design
document's own decisions (D1–D10 there) stand unless listed under
[Departures](#departures-from-the-design-document). Numbering here is
independent (`K-nn`). K-25 onwards are the
[metrics screen](#metrics-openspecchangesadd-metrics-dashboard).

## Open questions from the design, now answered

| Question | Decision |
|---|---|
| Copilot pane placement | **Right rail**, hidden at start so the browser is primary. `c` toggles, `tab` focuses. About 42% of the width (34–80 columns). |
| One provider adapter or two | **Two protocols, four presets**: Anthropic Messages, and OpenAI-style chat completions (`openai`, `openrouter`, `openai-compatible` for local/self-hosted). Plus the scripted stub for tests. Two real protocols is what actually proves the loop is neutral. |
| Default refresh interval | **5 s**, listing view only, `--refresh` / `refresh_interval` to change, `0`/`off` to disable. |

## Departures from the design document

### K-01 — Provider adapters use `net/http`, not vendor SDKs
The proposal lists "at least one concrete provider SDK". Both adapters are ~150
lines of plain HTTP+JSON instead.
*Why:* the surface needed is one request/response shape per protocol; SDKs
would add a large dependency tree to a binary that holds cluster credentials
and an API key, and tie request semantics (retries, timeouts) to code we do
not control. The adapters are tested against `httptest` servers.
*Cost:* no streaming, no SDK conveniences; new API features need hand-written
support.

### K-02 — `scale` patches the scale subresource through the dynamic client
Design D7 says "apps/v1 scale subresource" (implying the typed scale client).
It is still the `scale` subresource, but written as a JSON merge patch
(`{"spec":{"replicas":N}}`) via the dynamic client.
*Why:* it lets all four verbs share **one** write primitive
(`kube.Cluster.MergePatch`), which is what makes "there is exactly one
mutating call in the program" a checkable statement. Verified against a real
API server (dry-run) — see docs/testing.md.

### K-03 — Stale-state check instead of `resourceVersion` preconditions
The design's risk section suggests resourceVersion preconditions. Workloads'
`resourceVersion` changes on every status update, so a precondition captured
when the dialog opened would spuriously fail after a human-length pause.
Instead, `Apply` re-reads the object and compares **the specific values shown
as "before"** (replica count, the restart annotation, each affected
label/annotation key), and that it is still the same object (UID). If anything
moved, it refuses with a `StaleError` that says what changed. A few-millisecond race remains; the outcome is still the
approved "after" state.

### K-04 — Partial discovery is shown with a warning, total failure is not shown
The spec says a discovery failure must not present "a partial or misleading
type list". A single dead aggregated API (stale `metrics.k8s.io`) makes
discovery return an error *and* every other group; refusing to work at all
there would make the tool useless on many real clusters, and kubectl carries
on too. So: total failure → error screen, no list. Partial failure → the
discovered types **plus a visible warning naming the groups that did not
answer**. Partial-but-labelled is not misleading; partial-and-silent would be.

## Safety decisions

### K-05 — Approval is carried by an unforgeable value
`Plan.Apply` takes an `approval.Grant`. `Grant` has only unexported fields and
is created in exactly one place: the approve branch of `Gate.Ask`. It is bound
to a digest of (plan id, API server, exact request), so approving one proposal
cannot authorise another. This turns "nothing is applied without approval"
from a property of control flow into a property of types.

### K-06 — Chord keys to decide, type-ahead guard, `esc` hides
Approve is `ctrl+y` then `enter` (with 250 ms between them); reject is
`ctrl+n`; the dialog ignores input for 700 ms after appearing; `esc` hides the
dialog without deciding and `ctrl+p` reopens it from any pane. *Why:* the
realistic way to answer by accident is to be typing when the dialog appears.
The first version used `y`/`enter` and `n`; an independent review showed that
typing "why", enter, into what you thought was the copilot input approved the
change. Chords cannot be produced by prose. The 700 ms guard is not a timeout
in the spec's sense: nothing proceeds when it elapses.

### K-07 — An approved apply is not interrupted by cancellation
Once the human approves, the apply runs under a context detached from the
turn's cancellation (60 s limit). *Why:* cancelling mid-request would leave it
unknown whether the change landed, and unknowable outcomes are exactly what
the audit trail exists to prevent. Before approval, cancellation always wins
and nothing is applied.

### K-08 — No audit trail, no proposals
If the audit file cannot be opened, or any append fails, every later mutate
call is refused before the human is even asked (reads keep working). The spec
only requires reporting the failure and not treating the action as audited;
failing closed is stricter and matches "I never have to wonder". If the append
fails *after* a change was applied, the UI and the model are both told, in
capitals, that the change is not in the trail.

### K-09 — Edit means re-plan, re-validate, re-approve
An edit returns new arguments to the loop, which builds a fresh plan, dry-runs
it and opens a new dialog. The original proposal is recorded as
`edited / superseded`, the new one gets its own entry. An edit can never apply
directly, and cannot get around validation.

### K-10 — Refusing what is out of scope
There are no tools for delete / apply / patch / image / exec / drain, so the
model cannot call them. If it tries a name like `delete_pod`, the registry
answers with a refusal that names the unsupported operation and lists what
does exist. Name recognition is only for the quality of the message; an
unrecognised name is refused the same way. These attempts are not audit
entries (no proposal against the cluster ever existed); they are visible in
the transcript.

### K-11 — Audit format
- **Write-ahead for approvals** (a departure from the task wording "one entry
  per approved … action"). Rejected, dry-run-rejected, edited and cancelled
  proposals get one entry. An **approved** proposal gets two with the same
  `proposal_id`: `approved / applying`, fsynced *before* the request is sent,
  then `approved / applied` or `approved / failed`. If the first cannot be
  written, the request is not sent. *Why:* with a single entry written
  afterwards, a crash, a kill or quitting during a slow request left a live
  change with no record at all — the one thing the trail must never allow.
  (This was the first version; an independent review flagged it and it was
  changed.)
- **Canonical-line verification.** Besides the hash chain, a stored line must
  equal byte-for-byte the canonical encoding of what it parses to — so an
  added field or changed whitespace is detected too.
- **Head anchor sidecar** (`audit.jsonl.head`). A hash chain cannot show that
  whole entries were removed from the end. The sidecar holds the newest
  seq+hash; a log shorter than its head is reported, and so is a log with
  entries but no head. It is replaced on each append; it is not an entry.
- **The path must still be the open file.** Each append checks that
  `audit.jsonl` on disk is the file k8s-copilot holds open; if it was deleted or
  rotated away the trail is marked broken (and proposals stop) instead of
  writing into an unlinked file.
- **File lock on append** so concurrent instances keep a single chain.
- **Extra outcomes** beyond the spec's four: `applying` (above), `superseded`
  (edited) and `cancelled` (request ended while waiting) — so that every proposal a human
  ever saw has an entry.
- Location `$XDG_STATE_HOME/k8s-copilot/audit.jsonl`, directory 0700, file 0600.

### K-12 — Secrets are redacted for the model, not in the browser
Tool results replace Secret values with a marker. The browser shows the object
as the API returns it, as kubectl does, because that data is already yours and
stays on your machine; what goes to a third-party API is a different matter.

### K-13 — All cluster- and model-originated text is sanitised before display
See docs/security.md (terminal injection).

### K-14 — Guard-rails beyond the curated verb set
`max_replicas`, the managed-key deny list, the 20-key limit, the resolved-type
check for `scale`/`rollout_restart`, the paused-Deployment check, optional
`protected_namespaces`, and the per-case warnings. None is required by the
spec; each closes a way in which a "small reversible change" could be neither.

### K-15 — Structural properties are tested from the source tree
`internal/guard` parses the module and fails on: any subprocess-capable
import, a client-go client import outside `internal/kube`, a write call other
than the one `Patch`, a raw HTTP request in `internal/kube` that is not a GET,
`Apply`/`Ask`/`Append` called outside the gated path, a
`Grant` literal outside `approval`, a timer in the gate, a destructive file
call in `audit`. This is task 5.5 ("verify by test and code review") made
permanent.

## Other decisions

### K-16 — Server-side Table printing for columns
Design D2 asks for "per-type column hints derived from discovery". The API
server's Table response *is* that, for every type, maintained by the type's
authors. Generic NAME/STATUS/AGE remains as the fallback (and is what the
fakes exercise).

### K-17 — Typed tool arguments, strict decoding, `reason` required on mutations
Unknown fields are errors (so `{"replicas":3,"image":"x"}` is rejected, not
half-obeyed). Mutate tools require a `reason`, shown to the human as the
model's words.

### K-18 — Configuration
defaults < file < `K8S_COPILOT_*` < flags. Nothing is required beyond an API key in
the provider's usual variable. Unknown keys in the file are errors.

### K-19 — No streaming in v1
Replies arrive whole; the pane shows a spinner and each tool call as it
happens. Streaming is an adapter-level addition later and does not affect the
loop contract.

### K-20 — Conversation memory
History is kept for the session (`ctrl+l` clears). Once tool output in the
history exceeds ~300 kB, the oldest tool results are replaced by a marker;
message structure is preserved so providers keep accepting it.

### K-21 — Logs
The log view tails 500 lines and keeps the newest 5000 in memory. The agent's
`get_logs` keeps the *end* of the output when it has to cut (the API's own
`limitBytes` keeps the beginning, which is the wrong half for a crash).

### K-22 — Default model
`claude-opus-5-5` for the Anthropic preset: diagnosis quality matters more
than cost per question here. Change with `--model`.

### K-23 — Module path
`github.com/fardani235/k8s-copilot`, from the repository's git user. Change in
`go.mod` if the repository will live elsewhere.

### K-24 — Independent review before hand-over
The approval path was given to a separate reviewer with no knowledge of the
design rationale. It found no way to apply an unapproved change, and twelve
other issues. Fixed: write-ahead audit (K-11), deleted/rotated audit file,
prose-typable approval keys (K-06), missing head anchor accepted, three
unsanitised text paths, `p` unreachable from the copilot pane and `esc`
discarding a waiting proposal, the dialog's last line hidden when scrolling, a
stale type-picker index, provider redirects, the UID check (K-03), and the
raw-HTTP blind spot in the guard tests. Left as documented limits: the unkeyed
hash chain and the 1000-event read (docs/security.md).

## Metrics (`openspec/changes/add-metrics-dashboard`)

The full reasoning, with the alternatives, is in that change's `design.md`
(D1–D11). This is the record of what was decided.

### K-25 — Usage comes from `metrics.k8s.io`, read with the dynamic client
Two `list` calls (`nodes.metrics.k8s.io`, `pods.metrics.k8s.io`) through the
client `internal/kube` already holds. *Why:* it is the standard source (what
`kubectl top` reads), it needs only `list`, and it needs no new dependency —
`go.mod` is unchanged and the guard tests pass untouched. *Rejected:* the
kubelet Summary API (needs `nodes/proxy`, a permission that should not be
asked for), Prometheus (has to be configured; "nothing to set up"), the typed
`k8s.io/metrics` client (a dependency to parse two shapes), a
`SelfSubjectAccessReview` pre-check (a `create`; and trying the read is the
truth, asking is a prediction).

### K-26 — Usage is always shown against a bound: four reads, each with its own status
Usage is joined with `nodes` (allocatable) and `pods` (requests, limits, node,
phase, restarts). *Why:* "how loaded" is a ratio, and "CPU-saturated or out of
memory?" is a question about a limit — the copilot could not answer it from
usage alone. The four reads fail independently in real clusters, so each
carries its own status and the screen says which is missing. The two core
reads are served from the API server's cache (`resourceVersion=0`).

### K-27 — Unknown is a value of its own, and a missing source is not a table
`metrics.Amount` has an `OK` flag and its zero value is *unknown*. A missing
reading is `—`; a bound that is not set is `no lim` / `no req` / `none`; a
measured zero is `0`. When the source a listing is made of is unavailable the
listing has **no rows and no columns** — only the status. A table of dashes
under a warning banner was rejected: at a glance it is still a table.

### K-28 — Six ways to be missing, classified by attempting the read
Absent (404: not installed), Unavailable (503/500: registered, not
answering), Denied (403), Unauthenticated (401: credentials not accepted),
Timed out, Failed (anything else, including a 429). Each has its own wording,
written once in `internal/metrics`. No discovery lookup and no access review:
the read itself is authoritative.

### K-29 — One model, two renderers
`internal/metrics` turns a `Snapshot` and a `Query` into a `Listing` that
already holds the formatted cells, percentages, heat, caveats and the
"unavailable" status. The screen adds colour and layout; `get_metrics` prints
the same cells as text. Neither computes a number, so they cannot compute it
differently. This is `kube.Summarize`'s idea (one function, shown twice) made
stricter, because here the divergence risk is in the formatting itself.

### K-30 — One cluster-wide read, and everything is cut from it
`kube.Cluster.Metrics` keeps one cluster-wide read. Node readings always come
from it; pod readings too, a namespace's by narrowing. Only when the cluster
refuses the pods of the whole cluster are a namespace's pods read separately —
and joined to the same node readings. The tool accepts readings up to 15 s
old, so with the screen open the copilot is handed what is on the screen; when
it has to read for itself, the screen takes up those readings as the tool call
ends. Concurrent callers share one detached read (20 s bound). Both renderings
state the sample's age, so the one residual difference — the screen refreshing
while the model is composing its answer — is visible.

*This replaced a per-scope cache* (a snapshot per namespace beside the
cluster-wide one), which the review showed could hand the copilot and the
screen different readings of the same pod. See K-38.

### K-31 — The hierarchy is three tabs and a drill-down path
Nodes / namespaces / pods at the top; `enter` goes node-or-namespace → pods →
containers; `esc` goes back up one level, onto the row it came from. *Why not
a tree:* two hierarchies (node → pod, namespace → pod) over the same pods, and
thousands of leaves. *Why not stacked panels:* four rows each at 80×24, nothing
at all beside the copilot. Sorted busiest-first; the cursor follows the row,
not the position.

### K-32 — Cluster-wide by default; the browser's namespace when refused
The screen opens on the whole cluster whatever the browser's namespace — the
hierarchy is the scoping mechanism. If pod metrics are refused cluster-wide it
falls back to the browser's namespace and says so on the screen; each refresh
asks for the whole cluster again, so it widens by itself if the refusal is
lifted. Per-node pod
counts are not shown in a namespace-scoped view (a partial count would read as
a total).

### K-33 — Refresh at the pace of the source; say how old the sample is
Every `max(refresh_interval, 10s)`, only while visible, never over an
outstanding read. metrics-server samples every 15 s; faster polling repeats
numbers. The title shows the age of the *sample* (the older of the node and
pod samples) and flags it past 90 s. A redraw timer keeps that age counting on
an untouched screen, including with refresh off.

### K-34 — Hold-over: 60 s, always marked
A refresh that gets no usage after one that did keeps the earlier readings on
show for at most 60 s, with the title and a note saying they are not current
and why; then the unavailable state replaces them. *Why:* a flaky source
should not flip the screen between data and error every ten seconds, and old
numbers should not outstay a warning nobody reads. It lives in the shared
cache, so the copilot gets the same marked snapshot.

### K-35 — Heat only against hard bounds; thresholds shared
Amber at 75 %, red at 90 % of allocatable or of a limit. Usage above a
*request* is normal and is not flagged. The same constants decide the screen's
colour and the tool's `elevated` / `HIGH`. A pod row takes its heat from its
containers — limits bind containers, not pods — and names the container.

### K-36 — `M`, and no configuration
Capitals open full screens (`A`). `m` is maximize; `u` would shadow the detail
view's half-page-up. No new settings: `refresh_interval` is honoured, with a
10 s floor for this screen.

### K-37 — Degrade in a fixed order; the renderer bounds itself
Columns have a drop order (bars, node, request percentages, absolutes; never
name, CPU, MEM); gauges go full → stacked → abbreviated → shares only; the
sample's age outranks the breadcrumb. `fitBox` guarantees the screen never
hands its pane more lines or wider lines than it has.

Found while testing this on a real terminal, and fixed although it predates
the change: the "copilot is not available" text was not cut to its pane, so a
long reason (a real `openrouter` configuration) pushed the pane's bottom
border off a 60×16 screen. `tui.TestUnavailableCopilotKeepsItsFrame`.

### K-38 — Independent review before hand-over
As with the approval path (K-24), the change was handed to a separate reviewer
with the four promises (honesty, consistency, read-only, robustness) and
without the design rationale, and asked to break it with reproductions.

**What held.** No input produced a table of rows or a zero from a missing
source, on the screen or in the tool; a measured zero stayed zero; held
readings were always marked and expired; no race, deadlock, goroutine leak or
panic under concurrent load and 1010 malformed-input variants; only `list`
requests on the wire; no overflow at any pane size from 0 to 130 columns with
wide, combining, bidirectional and control characters in names.

**What did not, and was fixed.** Every item below was reproduced, fixed, and
given a test that fails without the fix.

| Finding | Fix |
|---|---|
| A per-namespace cache entry could outlive a newer cluster-wide read: the screen showed a pod at 99 % of its limit, the copilot was told 16 %. Likewise held on the screen but "unavailable" from the tool; a namespace failure shadowing a later success | One cluster-wide read that everything is cut from (K-30). `kube.TestMetricsAreCoherentAcrossScopes` |
| With refresh off or an interval over 15 s, the tool read fresh numbers the screen did not have | The screen takes up the tool's readings when the call ends. `tui.TestScreenAdoptsWhatTheCopilotRead` |
| A running init container was shown with requests and limits of "none" and measured against the limits of containers not yet started — at 100 % of its real limit, unflagged | Init and ephemeral containers keep their declared bounds and are listed when running; a container the spec does not describe is `—`, not "none". `metrics.TestRunningInitContainerKeepsItsBounds` |
| A sample with no containers was shown as `0m` / `0%` (the code's own comment said this was right; it was not — nothing was measured) | No containers, no reading. `metrics.TestSampleWithoutContainersIsNotZero` |
| CPU was rounded up to a millicore per container before summing: 300 idle containers read 300m, and a node's reading disagreed with the sum of its pods | Nanocores throughout, rounded for display only. `metrics.TestSmallReadingsAreNotRoundedUpBeforeAdding` |
| "No pods in namespace X" for a namespace that had not been read, after the screen was limited to another one mid-session | The listing says the namespace was not read; the screen goes back to the top and says why; it widens again by itself when the refusal is lifted. `tui.TestMetricsDoesNotCallAnUnreadNamespaceEmpty` |
| With refresh off, "sampled 1s ago" stayed on the screen indefinitely | A redraw timer while the screen is showing (K-33). `tui.TestMetricsAgeKeepsCounting`; confirmed with the binary |
| In the 24-column pane the filter was invisible, typing it and afterwards | The filter and "N of M" go first on the line. `tui.TestMetricsFilterIsVisibleInANarrowPane` |
| The row under the cursor — by default the hottest — was not drawn in its heat colour | The selected style keeps the heat foreground. `tui.TestSelectedRowKeepsItsHeat` |
| The tool's size cap cut the caveats and legend off a long listing, whose header still claimed 200 rows | Caveats come before the table, and rows are dropped — and counted — to fit. `tools.TestMetricsToolKeepsItsCaveatsWhenShortened` |
| A pod's limit share hid a container at its limit beside a roomy neighbour | Row heat comes from the containers (K-35). `metrics.TestPodIsAsHotAsItsHottestContainer` |
| A cluster percentage over nodes whose capacity was only partly known; a metrics-only pod shown on node `<none>`; hostile quantities overflowing | `metrics.TestClusterShareNeedsEveryReportingNodesCapacity`, `TestPodKnownOnlyToTheMetricsAPI`, `TestAbsurdQuantitiesAreUnknown` |
| 401 explained as a missing permission; 429 as "metrics-server is down" | A sixth state; 429 is "failed" with the server's words (K-28) |
| The newest sample of either source vouched for both | The age is the older source's. `metrics.TestSampleTimeIsTheOlderSource` |
| `M` from a view opened from the metrics screen reset its drill-down; a metrics API returning empty pages with a continue token was followed for 20 s; a name with a newline could forge a row in the text the model reads; the "unavailable" wording was drawn unsanitised | `tui.TestMetricsKeyReturnsToTheOpenScreen`, `kube.TestMetricsPagingCannotLoop`, `metrics.TestTableTextCannotBeForged` |

The reviewer also named four tests that passed without proving what their
comments claimed; each was rewritten (`TestCopilotSeesWhatTheScreenShows`'s
heat check, `TestMetricsTextIsSanitized` checking stripped output,
`TestMetricsCallerCanStopWaiting` asserting no timing, and the "read once"
tests covering one scope only).

**Left as documented limits**: a local clock behind the cluster's makes an old
sample look new; long caveats are cut (with a marker) in a 12-line pane full
of rows; the summary above a filtered listing still describes the whole
level; a 429 costs client-go's own retries before it is reported; the
handling of running init containers was tested with constructed samples
only.

## Cluster health (`openspec/changes/add-cluster-health`)

The full reasoning is in that change's `design.md` (D1–D9). This is the
record of what was decided, and of the choices the design left open.

### K-39 — One pure model, `internal/health`, rendered twice
`health.Observe(*Observation) *Report` classifies typed pods, nodes,
Deployments, StatefulSets, DaemonSets, Warning events and the shared metrics
snapshot, with the outcome of each read, into findings and per-class states.
Every sentence the user or the model reads — class lines, the summary, "not
covered" — is written there once. The screen draws the `Report`;
`get_findings` prints `Report.Text`. *Rejected:* classifying in the TUI and
again in the tool (guaranteed drift, in the one place where "the copilot said
the cluster is fine" is the dangerous sentence); classifying inside
`internal/kube` (interpretation in the transport package, untestable without a
cluster).

### K-40 — Availability by attempting the read
A class's state comes from what its reads answered, classified with the
metrics screen's vocabulary (K-28): `watched`, `forbidden` (403, 401),
`unavailable` (404 for a core type, 5xx, timeout, anything else), `not
applicable` (no metrics API, or no metrics in the observation). A class some
of whose reads answered is *watched in part* and names what it could not read.
The zero `health.Read` is "never read", not "read, found nothing". *Rejected:*
a `SelfSubjectAccessReview` preflight — a prediction layered on an answer, and
a `create` request in a tool that only reads.

### K-41 — No state between observations
`Observe` has no `previous` argument and nothing is retained between
refreshes; a condition that clears simply stops being listed. *Rejected:*
dedup and transition tracking, which existed only for the dropped tray
notifier and would invite history in.

### K-42 — Stated thresholds, and the ones the design left open
Constants in `internal/health`, shown in each class's "covers" text on the
screen and in the tool's result; no configuration. *Rejected:* making them
configurable (an operator should not have to tune a screen to trust it).
Values chosen here, since the design did not fix them:

- **Restarting repeatedly**: more than 5 restarts (`RestartThreshold`),
  whatever the current state. CrashLoopBackOff is a finding at any count.
- **Not ready**: a *running* pod none of whose containers is ready for over
  5 minutes (`NotReadyGrace`), measured from the pod's ContainersReady/Ready
  transition. A pod already reported for a crash loop, a pull failure or
  restarts is not reported again as not ready. Shorter is a pod starting up.
- **Stuck waiting**: a container waiting for any reason other than
  ContainerCreating / PodInitializing (CreateContainerConfigError,
  RunContainerError, …) for over the same 5 minutes. Added after the review
  (K-45).
- **Warning events**: the last 15 minutes (`EventWindow`), grouped by object,
  reason and message, at most 50 groups shown with "showing N of M".
- **Usage**: the metrics screen's red, `metrics.HeatOf(pct) == High` (90 %)
  of a node's allocatable or a container's limit — the same function, not a
  copy of its number.
- **StatefulSets and DaemonSets** have no progress deadline, so "not
  progressing" is: fewer ready than desired *and* a pod they control not ready
  for over 5 minutes. Updates held back by `OnDelete` or a partition are
  deliberate and not reported. Without the pod list this cannot be judged, and
  the rollouts class says so rather than calling them fine.
- **Deployments**: `Progressing=False` (ProgressDeadlineExceeded) or
  `ReplicaFailure=True`; paused or complete ones never.

### K-43 — Typed, capped reads in `internal/kube`; cluster-wide, then the browser's namespace
Pods, nodes, Deployments, StatefulSets, DaemonSets and Warning events are
listed through the typed clientset, one `list` each, at most 500 rows
(`kube.DefaultListLimit`), each with a 10 s bound and its own status. A read
refused with 403 is retried for the browser's namespace and its class says so.
A read cut at the cap carries the number of rows cut (from the server's
remaining count when it gives one) and its class is *watched in part*, never
clear — the one place where the existing cap could otherwise produce a false
all-clear. Usage comes from `Cluster.Metrics` at its 15 s reuse, never a read
of its own. *Rejected:* the generic (unstructured) listing path, which exists
to display, not to classify; paging past 500 rows, which the design did not
ask for and which would make a refresh's cost unbounded on large clusters —
the cut is stated instead (see limits below); the events.k8s.io API (the core
events the browser already reads are the same objects).

*Open choices made here:* the observation is kept in **one slot** with the
fallback namespace it was read with, and handed out again within 15 s
(`kube.HealthFreshFor`) when that fallback fits — it fits if it is the same
namespace, if no cluster-wide read was refused (the fallback made no
difference), or if the caller asks for whatever is latest
(`kube.AnyFallback`, used by `get_findings` when the model names no
namespace). Each class's read has its own 10 s bound and they run side by
side, so a slow type costs at most 10 s and the report is published when
every read has answered or hit its bound; the screen meanwhile says it is
reading (the design's "still reading, then timed out" per class is this, at
the granularity of one report).

### K-44 — `get_findings` prints the same report, and cuts findings, never states
The tool calls `health.Observe` on the observation behind the screen (reused
up to 15 s) and prints `Report.Text`: the age, the summary, every class's line
with what it covers and its thresholds, the "not covered" list, then the
findings. When the result would exceed `max_result_bytes` it drops findings
from the end and says how many of how many, and how many of each class, were
left out; the class states are never cut. *Why:* the class states are what
stops "nothing is wrong" being read off an empty list; findings can be
summarised, the absence of a class cannot. *Rejected:* the generic byte cap
(`textutil.Truncate`), which would cut the report wherever the limit fell; an
error result when a class is not watched (as `get_metrics` does for a missing
source), because the other classes' findings are still the answer — the
unwatched class is a stated limit of it. The tool takes one optional
argument, `namespace`, the fallback for a refused cluster-wide read; left out,
it is whatever the screen last used, else the context's default namespace.

### K-45 — Independent review before hand-over
As with K-24 and K-38, the change was handed to a separate reviewer with the
code, the spec and four promises (honesty, consistency, read-only,
robustness) — not the design rationale — and asked to break it with
reproductions, on fakes only.

**What held.** Every unread state (forbidden, credentials rejected, timed
out, never attempted, caller gave up, metrics absent or held) produced no
findings and was never called clear; cut and namespace-limited reads always
made their class incomplete; only `list` requests; `-race` clean across the
seven packages; a stuck read abandoned at its bound even when the transport
ignores the deadline; hostile names sanitised and the screen inside its pane
at every size; classification as specified.

**What did not, and was fixed** (each with a test that fails without the fix
— checked by reverting it):

| Finding | Fix |
|---|---|
| Pods readable in one namespace only, StatefulSets cluster-wide: rollouts said "watched, no findings" although stalls elsewhere were never judged | A stated limit, "StatefulSet and DaemonSet stalls judged in namespace X only"; the class is not clear. `health.TestRolloutsWithPodsOfOneNamespaceAreNotClear` |
| Metrics API answering with no readings (metrics-server starting, or unable to reach the kubelets): usage — and the whole report — "No findings. Every class was watched" | Nodes and running pods without a reading are counted and stated; none at all is *unavailable*. `health.TestUsageWithoutReadingsIsNotClear` |
| Fitting the tool's text was quadratic: 5000 findings took 13 s (1m48 under `-race`) | Prefix sums and a binary search. `health.TestTextCutIsNotQuadratic` |
| On the health screen the focus's namespace was the selected finding's, so the model asked for a different fallback than the screen's and the one-slot cache thrashed | The focus's namespace is the browser's (the screen's fallback); the finding's namespace is stated with it; the tool's argument says to give it. `tui.TestHealthFocusText`, `kube.TestAnyFallbackTakesTheScreensObservation` |
| A budget smaller than the class states alone was exceeded | A compact head (each class line shortened, "covers" left out), every state kept. `health.TestTextStaysWithinBudgetWithLongReasons` |
| A container stuck in CreateContainerConfigError / RunContainerError, and a node with no Ready condition, were clear | Findings: any waiting reason but ContainerCreating/PodInitializing past the 5-minute grace; `NoReadyCondition`. `health.TestStuckWaitingAndMissingReadyAreFindings` |
| The focus did not say how old the reading was; a chain of reads (one out, then one's own) could outlast the screen's wait | The focus states the age; a caller waits for at most one read in flight and then makes at most one of its own, within `kube.HealthObservationBound`, which the screen's wait covers |

**Left as documented limits**: StatefulSets and DaemonSets whose pods were
never created, and pods stuck in ContainerCreating, are not detected (both now
named in "Not covered"); reads stop at 500 rows per type, stated, with no
paging; with nothing cached and no namespace given, `get_findings` falls back
to the kubeconfig context's namespace rather than the browser's; a
crash-looping pod is reported under workloads and events both; a local clock
behind the cluster's makes an old reading look new, as on the metrics screen;
the interactive checks against a live cluster (tasks 5.3, 5.4) were not run —
creating workloads in the cluster was not permitted to this session — and are
listed in docs/testing.md for a human.

## Not done, on purpose

- No metrics history, trends, graphs or alerts; no source other than
  `metrics.k8s.io`; no CPU/MEM columns in the ordinary listings (argued for as
  the next step in the metrics change's `design.md`).
- No delete / apply / patch / image / exec / cordon / drain (out of scope for
  this change).
- No in-app context switching, no multi-cluster.
- No scoped ServiceAccount (deferred by the proposal).
- No watch-based live updates; listings poll.
- No pagination beyond the first 500 rows of a listing (the UI says when a
  listing is cut).
