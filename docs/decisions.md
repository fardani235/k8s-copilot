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
