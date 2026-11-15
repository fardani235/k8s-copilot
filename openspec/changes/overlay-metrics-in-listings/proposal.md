## Why

The metrics screen answers "how busy is it right now", but the operator's first
stop is still the ordinary pods and nodes listing — and a pod's name, restarts
and age sit there with no indication of whether it is busy, starved or idle.
The metrics screen's own design named usage columns in these listings as the
contained follow-up once `internal/metrics` existed; it does now.

The column is cheap. The honesty rule is not. These listings already have to
say "you are not allowed" instead of showing an empty table; adding usage must
not undo that. When the metrics API is absent, broken, refusing or slow, the
listing must have **no usage column at all** — never a column of `—`, never a
column of zeros, and never a percentage of a bound that could not be read.

## What Changes

- The ordinary **pods** listing gains `CPU`, `%CPU/R`, `%CPU/L`, `MEM`,
  `%MEM/R`, `%MEM/L`; the **nodes** listing gains `CPU`, `%CPU`, `MEM`,
  `%MEM`. Values, percentages and the unknown marker are the metrics screen's
  own, from the same shared readings — no second read, no second computation.
- The columns are added **only when the readings are there and cover the
  listing**. Absent, unavailable, denied, unauthenticated, timed-out or failed
  metrics produce **no usage columns**, and the listing says in one line why.
  A namespace-scoped read never decorates an all-namespaces listing.
- A column is added **only if some listed row has a known value for it**: an
  all-unknown column is not drawn as a column of `—`. Per-row unknowns stay
  `—`; a measured zero stays zero.
- The listing states **how old the usage is**, and marks it when it is old or
  when a failed refresh has left earlier readings standing in — the same hold
  and freshness rules the metrics screen uses.
- **The copilot's `list_resources` result is the same table**: same columns,
  same values, same absence and same stated reason.
- On a narrow pane, the overlay columns give way before the listing's own
  columns; names, status and age are never dropped for usage.
- Filtering, selection and refresh of the ordinary listing are unchanged.
- **Read-only, no new cost**: the overlay reuses the shared metrics cache and
  joins a read already in flight; no new permission, dependency, configuration
  or cluster-side installation.

Not in this change: usage in other resource types, history or trends, sorting
the ordinary listing by usage, or any change to the metrics screen itself.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `cluster-metrics`: current usage is also overlaid on the ordinary pods and
  nodes listings, under the existing "unknown is never zero" and "say plainly
  when the source is unavailable" rules — including no column at all when the
  readings are missing.
- `resource-browser`: the pods and nodes listings present those columns after
  their own, preserve navigation/filter/refresh, and drop usage before their
  own fields on a narrow pane.
- `agent-copilot`: `list_resources` for pods and nodes carries the same overlay
  and the same stated absence, so the model and the screen see one table.

## Impact

- `internal/kube`: a shared overlay step over `kube.Table` (columns, freshness,
  absence reason) used by both the browser and the read tool; no new request
  path or write verb.
- `internal/metrics`: the formatting and freshness vocabulary grows no new
  source of truth; the overlay reuses `Snapshot`, `FormatCPU`, `FormatMem`,
  `Percent`, `HeatOf` and `Freshness`.
- `internal/tui`: the listing renders overlay columns, the one-line absence
  note, a freshness line that keeps counting, and width-aware dropping.
- `internal/tools`: `list_resources` prints the same overlay and note.
- Docs: README key/column notes, `docs/architecture.md`, `docs/decisions.md`,
  `docs/testing.md`, CHANGELOG.
- `go.mod`, the guard, the approval gate and the audit trail are unchanged.
