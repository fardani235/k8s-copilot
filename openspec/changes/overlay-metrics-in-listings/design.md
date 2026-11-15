# Design

## Context

See `proposal.md` for why. Three properties of the current code shape this.

- The ordinary listing is a `kube.Table`: columns normally come from the API
  server's Table printer (K-16), with a generic fallback. Two renderers consume
  it — `tui.viewList` and `tools.FormatTable` for `list_resources` — so an
  overlay added in one place can serve both.
- `kube.Cluster.Metrics` already owns one shared cache with a freshness
  window, in-flight sharing and hold-over for a failed refresh. The overlay
  must reuse it, not add a second read path.
- `internal/metrics` is pure and `internal/kube` imports it, so the overlay
  may live in `kube` (which knows `Table`) while the values, percentages,
  markers and freshness wording stay in `metrics`. The reverse would be an
  import cycle.

The guard tests are unchanged by this design: the overlay issues no request of
its own, no new raw HTTP, no write verb and no subprocess.

## Goals / Non-Goals

**Goals**

- One overlay implementation, one set of cells, for the browser and the
  copilot.
- Absence is a state carried by the table, never a column.
- No metrics request when the cache is fresh; no second request while one is
  in flight.
- The listing stays a listing: same rows, order, selection, filter and keys.

**Non-Goals**

- Usage in other resource types; sorting by usage; heat colouring of ordinary
  rows (the metrics screen owns that); history.
- A second sampling policy. The overlay is exactly as fresh as the metrics
  cache allows.

## Decisions

### D1 — The overlay is a post-processing step in `internal/kube`

New file `internal/kube/overlay.go`:

```go
// AttachUsage appends the usage columns to a pods or nodes listing.
func AttachUsage(t *Table, snap *metrics.Snapshot, namespace string) *Table
```

`Cluster` gains one entry point that both consumers call:

```go
func (c *Cluster) ListWithUsage(ctx context.Context, t ResourceType, namespace string,
    limit int, maxAge time.Duration) (*Table, error)
```

For every type it does what `List` did. For core `pods` and `nodes` it also
obtains the snapshot through the existing `Metrics` path (concurrently — see
D5) and attaches the overlay. The TUI and the read tool both call it, so
neither can have a different policy about when the columns appear.

*Why not inside `Cluster.List`.* `List` is also the generic path used for every
type and by tests; making it read metrics would add latency and failure modes
to listings that have nothing to do with usage.

*Why not in the TUI.* `list_resources` must return the same table; a
TUI-only overlay would give the model a different one.

*Alternatives rejected.* Building the table in `internal/metrics` (import
cycle); letting each renderer compute cells from the snapshot (two
computations, exactly what the existing "one source of truth" rule forbids).

### D2 — The cells come from the metrics listing's own constructors

To make "equal to the metrics screen" structural rather than careful, export
the exact cell builders and use them in both places:

```go
// PodUsageCells: CPU, %CPU/R, %CPU/L, MEM, %MEM/R, %MEM/L
func PodUsageCells(p *Pod) [6]string
// NodeUsageCells: CPU, %CPU, MEM, %MEM (of allocatable)
func NodeUsageCells(n *Node) [4]string
```

`listPods` and `listNodes` are refactored to fill their usage cells from these
functions, and the overlay calls the same ones. There is then one place that
decides that a missing bound is `no req` / `no lim` when the bound is known to
be absent, `—` when it is unknown, and that a measured zero is `0m` / `0`.

Lookup is by key: nodes by name, pods by `namespace + "/" + name`. The
snapshot is indexed once per attach, not scanned per row. A listing row that
the snapshot does not contain (a pod that finished after the sample, or was
created after it) gets `—`: the reading is unknown, and assuming it is zero
would be the one thing this change must not do.

### D3 — Only core pods and core nodes, and only when the readings cover the listing

The overlay applies when the resolved type is `group == ""` and
`resource == "pods"` or `"nodes"`. A custom resource that happens to be called
`pods` is not decorated.

Node columns require `snap.Sources.NodeMetrics.OK()`; pod columns require
`snap.Sources.PodMetrics.OK()` and coverage: the snapshot's pod scope is empty
(cluster-wide) or equals the listing's namespace. A namespace-scoped read
never decorates an all-namespaces listing, because most rows would then be
`—` — a column that looks like data and is not. This mirrors the metrics
screen's own rule that it says "showing only namespace X" rather than drawing
others.

Absence produces `UsageOverlay.Note` from the metrics vocabulary
(`SourceStatus.Headline()` plus scope), so the sentence is the same one the
metrics screen and `get_metrics` use. A coverage failure gets its own sentence
naming the namespace that was read.

### D4 — The overlay is metadata on the table, not a parallel structure

```go
type UsageOverlay struct {
    // From is the index of the first overlay column in Table.Columns.
    From int
    // Drop ranks the overlay columns; higher goes first (0 never).
    Columns []int
    // Note is the one line to show: why the columns are absent, or the one
    // caveat that qualifies them ("requests and limits unknown, so
    // percentages are not shown"). Empty when there is nothing to say.
    Note string
    // Snap is the snapshot the cells came from; nil when there is no overlay.
    Snap *metrics.Snapshot
}
```

`Table.Columns` and every `Row.Cells` are extended in place, so `FormatTable`
and `viewList` need no special casing to print values. `UsageOverlay` adds only
what the renderers cannot infer: where the overlay starts, drop order, the one
line, and the snapshot for freshness at render time.

A method `(*UsageOverlay) Line(now) (text string, warn bool)` produces the one
line, so the browser and the copilot print the same words:

- absent: the reason (`Note`);
- present and fresh: `CPU/MEM sampled 8s ago · 15s window`;
- present and old or held: the `metrics.Freshness` warning text (`Warn` true).

Storing the snapshot, not a pre-formatted age, is what lets the age keep
counting without a cluster read.

### D5 — Read concurrently, with a short overlay budget; the read continues if abandoned

Waiting for a hung metrics API must not delay a listing. `ListWithUsage`
starts the listing and the metrics read together (a `sync.WaitGroup`, the same
shape as `readMetrics`) and returns one table, so the columns never flash in
later and the two consumers cannot order them differently.

The metrics call gets a child context with `UsageOverlayWait = 3s`, while the
listing keeps its own 30 s. `metricsCache.get` deliberately detaches the read
(`context.WithoutCancel`) and bounds it at `MetricsTimeout`; so a slow source
yields an "abandoned" snapshot now (no columns, "did not arrive in time") and
a completed, cached read a few seconds later. The next listing refresh — five
seconds by default — finds it fresh and shows the columns without waiting.

*Why not the full 20 s.* A listing is a navigation key; three seconds is
already a long pause. *Why not zero (never wait).* A cold cache would then
never produce columns from the listing.

`maxAge` is `kube.MetricsFreshFor` (15 s), the same window the copilot tool
uses. A 5-second listing refresh therefore usually serves from cache and only
starts a new metrics read when the sample would be older than the window.

### D6 — A column with no known cell is not added

After the cells are computed, an overlay column is dropped if every listed row
holds `metrics.NoData` for it. This is the rule "never a column of `—`"
applied to the realistic partial case: usage readable but pod or node objects
unreadable, so every percentage would be unknown. `no req`, `no lim` and
`none` are facts, not missing readings, so a column made of them is kept.

The reason is folded into the single `Note` line, for example: "Node capacity
is unknown (…), so usage is shown without percentages."

### D7 — The listing's filter keeps matching only what it matched before

`viewList`'s filter joins every cell of a row. After the overlay, a filter of
`5` would match a `15m` reading and reorder what the user sees for reasons
unrelated to the resource. `refilter` therefore joins `r.Cells[:overlay.From]`
when an overlay is present. Sorting, selection keys, `enter`/`l`/`e` and the
row tint are untouched.

### D8 — Width: usage gives way, never the listing's own columns

`viewList` measures natural widths (`Table.Columns`, capped at 56) and clips
to the pane. With six extra columns a narrow pane would silently cut the
overlay off the right edge, which is worse than not showing it. The renderer
now measures before drawing and drops *overlay* columns while the line is too
wide, in this order: request/limit percentages first, then `MEM`, then `CPU`.
Server columns are never dropped; when even they overflow, today's clipping
applies. Overlay drops are silent: the values are available, the pane is not,
and no sentence can fix that.

The copilot has no width, so `FormatTable` prints every overlay column.

### D9 — Freshness needs a clock of its own on the browser

The metrics screen already runs a five-second redraw timer so its stated age
keeps counting. The browser listing had no such timer because everything in it
was fetched at refresh time. With an overlay, a screen left alone with refresh
switched off would show "sampled 8s ago" for ever.

A `listClockMsg` mirrors the metrics clock: started when a listing with an
overlay is drawn, retired when the view changes or the overlay goes away, and
carrying no cluster read — it only makes the runtime redraw so `Line(now)`
recomputes the age and the stale/held flag appears on time.

### D10 — Held and stale readings keep their columns and say so

`metricsCache` already returns the last good snapshot, marked `Held`, while a
failed refresh is within `MetricsHoldFor`. The overlay keeps those columns and
`Line` returns the hold sentence with `warn` set; after the hold expires the
snapshot has no usage, so the columns disappear and the reason replaces them.
A sample older than `StaleAfter` is likewise marked in place. This is the same
behaviour, and the same words, as the metrics screen.

## Risks / Trade-offs

- **Browsing now costs metrics reads.** A user who only looks at pods triggers
  one wide metrics read when the cache is cold (two `list` requests). →
  Bounded by the 15 s freshness window and shared with the metrics screen and
  the copilot; no polling while another view is shown.
- **A slow metrics API pauses a listing by up to 3 s.** → The short budget, the
  detached read and the cache mean only the first refresh pays; the listing
  itself still succeeds.
- **Six extra columns on a 60-column terminal.** → D8's drop order keeps the
  resource's own fields; the metrics screen remains the place for full detail.
- **Values could drift between the listing and the metrics screen.** → D2
  makes the cell construction literally the same function, not a
  re-implementation.
- **Finished pods show `—`.** They are absent from the metrics snapshot by
  design. Treating them as zero would infer a reading; `—` is the honest
  answer, and the row's status still says `Succeeded` / `Failed`.
- **A column of `no lim` for every pod** is noisy but true, and distinguishable
  from `—`; no special case is added.

## Migration Plan

No stored data, configuration, permission or dependency changes. Deploy is a
build. Rollback is removing the `AttachUsage` call from the two consumers; the
metrics screen and `get_metrics` are unaffected either way.

## Open Questions

None that change the specs or the task breakdown. The exact labels of the
request-share and limit-share columns will match the metrics listing's
(`%CPU/R`, `%CPU/L`, `%MEM/R`, `%MEM/L`) rather than inventing shorter ones, so
that a glance transfers between the two screens.
