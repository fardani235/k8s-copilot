# Design

## Context

See `proposal.md` — Why, and `specs/cluster-health/spec.md` for the behavior
this must satisfy. The constraints below are properties of the code as it
stands, and each one shapes a decision.

- **`internal/metrics` established the pattern this change follows**: one pure
  model (`Snapshot` → `Query` → `Listing`), rendered by both the screen and the
  read tool, so the two cannot disagree (K-29, K-37). The health capability has
  the same two consumers and the same risk of divergence.
- **Unknown is a value of its own here** (K-27). The metrics screen already
  refuses to draw an unavailable source as an empty table. A health view has a
  worse failure mode: an unread class rendered as "clear" is an active lie about
  cluster health.
- **`internal/kube` is the only package that talks to the API**, and the guard
  asserts exactly one write call, one raw HTTP GET, one approval call site, and
  no subprocess. Health adds four-ish reads and must add no write path.
- **The listings cap at 500 rows** (`kube.DefaultListLimit`). A cluster-wide pod
  list on a busy cluster can exceed that, and a truncated read that is not
  stated would silently hide findings.
- **The tray change was dropped.** Its pure package would have been
  `internal/notify` with transition state and dedup, because a notification is
  an event. A screen and a tool render *current* state. That is the single
  largest simplification available here, and this design takes it.
- **`cluster-metrics` is not yet in `openspec/specs/`** — its change is 38/40
  and unarchived. That constrains how this change may touch shared
  requirements; see D7.

## Goals / Non-Goals

**Goals**

- One classification of one observation, rendered three ways over time (screen,
  tool now; a headless report later), with no second implementation.
- Every class independently able to say "not watched, and why", and no path by
  which a class that failed to read contributes a finding or an all-clear.
- A finding that is actionable in place: it names an object the browser can
  open, and a key opens it.
- The same honesty rules as the metrics screen for truncation, age, partial
  permissions and absent sources.

**Non-Goals**

- Any persisted state: no history, no transition tracking, no notification
  history, no "first seen".
- Any new source. Findings come only from pods, nodes, the three rollout
  workloads, Warning events, and the metrics snapshot the tool already reads.
- A general rule engine or user-defined alert rules.
- Correlation between findings: a crash-looping pod that also emits Warning
  events will produce both. V1 does not merge them.
- Classes this change does not cover: failed Jobs/CronJobs, Pending pods that
  cannot schedule, PVC problems, HPA state, PodDisruptionBudgets, endpoint
  health, and anything requiring a component that is not already read.

## Decisions

### D1 — A new pure package `internal/health`, with two renderers

`internal/health` holds the model only: `Missing`/`State`, `Finding`, `Class`,
`Report`, the classification functions and every sentence the user reads. It
takes a value — typed pods, nodes, workloads, events, an optional metrics
snapshot, and the status of each read — and returns findings plus per-class
state. It has no client, no clock of its own, no I/O.

```
 kube reads (each with its own status)
   pods | nodes | deployments | statefulsets | daemonsets | warning events | metrics
        |
        v
   health.Observation ---> health.Report { Findings, Classes }   (pure)
        |                              |
        |                              +--> TUI health screen
        |                              +--> get_findings tool (text)
        |                              +--> (later) a headless report
```

*Why.* The spec requires the screen and the copilot to be unable to differ
(one set of findings for both). The only reliable way to satisfy that is a
single computation both consume — the rule already proven for metrics.

*Alternatives rejected.* Classifying inside the TUI and again inside the tool:
guaranteed drift, and the drift would be in the one place where "the copilot
said the cluster is fine" is the dangerous sentence. Classifying inside
`internal/kube`: puts interpretation in the package whose job is transport, and
makes the classification untestable without a cluster.

### D2 — Availability comes from attempting each read, not from predicting it

Each class's read is attempted and its outcome classified, exactly as the
metrics screen classifies its six missing states (K-28): not served, not
answering, forbidden, credentials rejected, timed out, other. The class state
is derived from those outcomes. `SelfSubjectAccessReview` is not used.

*Why.* An access review *predicts*; the read *answers*. The two can disagree —
aggregated roles, admission, a namespace-scoped binding — and a health screen
that predicted "forbidden" for something it could actually read, or worse
predicted "allowed" and then suppressed the failure, would be less trustworthy
than one that reports what happened. It also keeps the metrics screen's
vocabulary, so a user who learned "the metrics API is not answering" there reads
the same words here.

*Alternative considered.* An SSAR preflight, which would let the screen say what
is unwatched before spending the reads. Deferred: it is a prediction layer on
top of answers, and the same information is available by just trying. If it is
ever added, it belongs in the headless diagnostic, not in the health model.

### D3 — One observation, no previous state

`health.Observe(obs)` is a pure function; there is no `previous` argument and
nothing is retained between refreshes. A condition that clears simply stops
appearing.

*Why.* Dedup and transition detection existed only because the tray needed to
notify on change. With no notifier, retaining state buys nothing and costs an
API that invites history to creep in. This keeps the package honest: the report
is a picture of now, and it says how old the picture is.

*Consequence.* A user watching the screen sees a finding disappear when it
clears, with no "resolved" line. That is consistent with the metrics screen and
is the behavior the spec states.

### D4 — The screen is one list with classes, not tabs

The health screen is a single scrollable list of findings, ordered by class
(workload, node, rollout, usage, events) and then by severity and name. Above
it, a summary line states how many findings there are and which classes are
watched. `/` filters by text across object name, namespace, class and reason.
`enter` opens the existing detail view; `l`, `e` and `d` open logs, events and
detail for the selected finding's object, as they do on the metrics screen.

*Why.* Findings are few by definition — a cluster with a hundred of them has a
different problem. Tabs (the metrics screen's approach) are right for a
hierarchy the user explores; here the question is "what is wrong", and one page
answers it. Reusing the existing keys means no new vocabulary to learn.

*Alternative rejected.* A per-class tab bar mirroring the metrics screen. It
would hide two thirds of the answer behind a keypress and require a second
notion of "selected row across tabs".

### D5 — Thresholds are stated, not implied

Where the system has to choose a line — "restarting repeatedly", "not ready
for", "recent Warning events" — the chosen value is shown in the screen's
footer or its class line, and in the tool's result. The line is a small set of
constants in `internal/health`, not configuration.

*Why.* An unexplained threshold makes a correct finding look arbitrary and a
missing finding look like a bug. Saying "restarted more than 5 times" turns a
judgment into a fact the user can act on. No new configuration keeps the
"nothing to set up" property; the values are not the kind of thing an operator
should have to tune to trust the screen.

*Usage thresholds are not new.* The "hot" boundary for nodes and containers is
the existing `metrics.HeatOf` threshold — amber 75%, red 90% — so a "hot"
finding and a red row on the metrics screen mean exactly the same thing. This
change adds no second definition of heat.

### D6 — Typed reads in `internal/kube`, one per class, cluster-wide then namespace

`internal/kube` gains typed listing helpers returning typed values
(`[]corev1.Pod`, `[]corev1.Node`, `[]appsv1.Deployment`, …), not unstructured
objects, because classification needs conditions, container states and
restart counts. Each helper takes the usual timeout, reports its own error, and
follows the metrics screen's fallback: try cluster-wide, and when that read is
refused, retry for the browser's namespace and say that is the scope.

*Why typed.* The generic listing path exists to *display* whatever the API
serves. Classification is a different job and wants the typed structs; the
metrics package already decodes typed pods for exactly this reason, and reusing
that decode is cheaper than a second one.

*Consequence for the 500-row cap.* A typed helper returns the number of rows it
cut. A truncated read is reported to the health model as truncated, and the
affected class says so, so a finding that was not in the first 500 rows can
never be presented as "nothing found". This is the one place where the existing
listing cap could quietly produce a false all-clear.

### D7 — Focus and capability deltas avoid the in-flight metrics change

The health screen sets the focus the way the metrics screen does (a new view
value and a description naming the screen and the selected finding), but the
delta spec expresses it as a new **ADDED** requirement rather than a
`MODIFIED` of "Expose current focus to the copilot".

*Why.* `add-metrics-dashboard` is unarchived and already modifies that
requirement and "Keyboard-driven layout with copilot pane". Two unarchived
changes modifying the same requirement block lose one another's text at archive
time: whichever archives second replaces the block with its own copy. Adding
new requirements avoids the collision regardless of archive order.

*Consequence.* When both changes are archived, the focus and keyboard
requirements will each exist twice — once as the modified block, once as this
change's added requirement. That is acceptable for behavior that is genuinely
additive (a new screen's focus and keys), and it is visible in the spec rather
than lost. If the metrics change is archived first, a later consolidation pass
can fold them together; it is not required for correctness.

### D8 — Reads happen only while visible, bounded, and never overlapping

The refresh tick follows the existing `refresh_interval`, with a floor of 15
seconds (metrics uses 10; health makes more calls). A read in flight blocks the
next tick. Leaving the screen stops the polling. Each class read carries a
bounded timeout, and one slow class does not hold the others: the report is
assembled from whatever has completed, with the outstanding class marked as
still reading and then as timed out.

*Why.* Four cluster-wide lists every few seconds is real API load, and a
background screen the user left open is the wrong place to spend it. Per-class
timeouts keep a single unresponsive resource type from turning the whole screen
into "reading…".

*Age.* The age shown is the oldest of the reads behind the report, measured
from when the local process received it. A local clock that is wrong therefore
cannot make an old sample look new — the same limit the metrics screen
documents.

### D9 — The tool renders the same report, capped, and says what it cut

`get_findings` calls the same `health.Observe` on the same observation and
renders the report as text within the existing `MaxResultBytes` cap. When it
cuts, it keeps every class's state and the count, and drops findings — never
the other way round.

*Why.* The classes' state is the part that prevents a wrong answer ("nothing is
wrong") from being drawn from an empty list. Findings can be summarized; the
absence of a class cannot.

## Risks / Trade-offs

- **A truncated pod list hides a finding** → the class read reports the cap and
  the class states that it covered only the first rows; the screen never says
  "clear" from a truncated read.
- **Warning events flood a busy cluster** → one stated window, grouped identical
  warnings with a count, a render cap, and "showing N of M".
- **Double reporting**: a crash-looping pod produces both a workload finding and
  Warning events → accepted in v1 and stated in the screen's wording; findings
  name the object so the duplicate is recognizable rather than confusing.
- **A class that is genuinely empty looks like a class that failed** if the
  status rendering is sloppy → the pure model carries a distinct state for each;
  a test asserts each state renders differently, and that "watched with no
  findings" is never rendered as "not watched" or vice versa.
- **False confidence in what is covered** → the screen states the classes it
  covers; the docs list what it does not (Jobs, scheduling, PVCs, HPA, PDB,
  endpoints). The tool's instructions repeat the limit.
- **Cost on large clusters** → four cluster-wide lists at a 15 s floor, only
  while visible, each bounded; metrics reuse the existing cache rather than a
  second read.
- **Thresholds are judgments** → they are stated on screen; the usage threshold
  is the metrics screen's, not a new one.
- **Time and clock skew** → age is measured locally from receipt; documented,
  as on the metrics screen.

## Migration Plan

None required. No new configuration, no persisted state, no change to the
audit format, the approval path, the provider configuration, or the wire
behavior of any existing tool. The binary gains a screen and a read tool; a
user who never presses the health key sees no difference. Rollback is removing
the key and the tool — the model package is inert without a caller.
