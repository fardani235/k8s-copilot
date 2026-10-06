# Decision log

Decisions made while implementing `openspec/changes/add-k2stui`. The design
document's own decisions (D1–D10 there) stand unless listed under
[Departures](#departures-from-the-design-document). Numbering here is
independent (`K-nn`).

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
  `audit.jsonl` on disk is the file k2stui holds open; if it was deleted or
  rotated away the trail is marked broken (and proposals stop) instead of
  writing into an unlinked file.
- **File lock on append** so concurrent instances keep a single chain.
- **Extra outcomes** beyond the spec's four: `applying` (above), `superseded`
  (edited) and `cancelled` (request ended while waiting) — so that every proposal a human
  ever saw has an entry.
- Location `$XDG_STATE_HOME/k2stui/audit.jsonl`, directory 0700, file 0600.

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
defaults < file < `K2STUI_*` < flags. Nothing is required beyond an API key in
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
`github.com/fardani235/k2stui`, from the repository's git user. Change in
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

## Not done, on purpose

- No delete / apply / patch / image / exec / cordon / drain (out of scope for
  this change).
- No in-app context switching, no multi-cluster.
- No scoped ServiceAccount (deferred by the proposal).
- No watch-based live updates; listings poll.
- No pagination beyond the first 500 rows of a listing (the UI says when a
  listing is cut).
