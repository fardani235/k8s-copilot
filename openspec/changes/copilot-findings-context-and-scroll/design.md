# Design

## Context

See `proposal.md` — Why, and the two spec deltas for the behavior required.

- **The report already exists and has two renderers.** `internal/health` turns one `health.Observation` into one `health.Report`; `internal/tui/health.go` draws it and `internal/tools/health.go` prints it with `Report.Text`. This change adds a third consumer of the same report, not a second classification.
- **`kube.Cluster.HealthObservation` is the sharing point.** It keeps one observation in one slot and hands it out again for up to `kube.HealthFreshFor` (15 s) when the requested fallback fits. `kube.AnyFallback` means "whatever the latest observation used", which is what the screen's report is. `get_findings` already relies on this (K-44).
- **The transcript is already a `viewport.Model`.** It is sized in `layout()` and receives `pgup`/`pgdown`/`ctrl+u`/`ctrl+d` only from `onCopilotKey`. `renderChat()` calls `GotoBottom()` on every update. The program runs with `tea.WithAltScreen()` only — no mouse reporting.
- **The model is not obliged to call any tool.** System-prompt instructions improve the odds but cannot guarantee it. A model that ignores `get_findings` falls back to listings until `MaxToolCalls` (30 by default) runs out.

## Goals / Non-Goals

**Goals**

- A health question is answerable from the context already sent with the turn, with no `list_resources` discovery pass.
- The snapshot is the same classification the screen shows, not a parallel one, and says its age and what it could not cover.
- The snapshot is bounded in bytes and in time, and its absence is stated rather than silent.
- The transcript can be scrolled by the input device the user reaches for first, and the reader's position is visible and stable.

**Non-Goals**

- No new health classes, thresholds, sources or configuration.
- No change to the `get_findings` tool's contract, the mutation set, the approval gate or the audit trail.
- No mouse support for the browser listing, detail/logs/events body or approval dialog.
- No text-selection work beyond what the terminal does with mouse reporting on.

## Decisions

### D1 — The snapshot goes in the request's system context, once per turn

`agent.Run` builds `system` once, before the first provider call, and uses it for every iteration of that turn. This change appends a health block to that string. The block is not stored in `a.history`.

*Why.* The snapshot is per-turn context, not conversation. Prepending it to the user message would store a fresh 6 KB copy in history on every turn, accelerate the `historyBudget` eviction and make old snapshots look like things the user said. A tool result would need a tool call, which is the problem being solved. Sending it in the system context keeps history clean and gives every iteration of the turn the same snapshot.

*Alternative rejected.* Injecting it as a synthetic user message. Simpler to implement, but it both bloats history and lets an old snapshot sit in the conversation as if it were current.

*Consequence.* The snapshot's bytes are paid on every model iteration of a turn, as the rest of the system prompt already is. The budget (D3) keeps that bounded.

### D2 — The snapshot is the screen's report, from the shared observation

The `agent.Config` gains:

```go
// Snapshot returns the bounded text of the current classified findings,
// or an error when it cannot be taken. nil disables the block.
Snapshot func(ctx context.Context) (string, error)
```

`cmd/k8s-copilot` implements it with:

```
obs := cluster.HealthObservation(ctx, kube.AnyFallback, kube.HealthFreshFor)
text := health.Observe(obs).Text(time.Now(), agent.HealthSnapshotBytes)
```

*Why.* This is the exact path `get_findings` uses, with `AnyFallback` so a snapshot reuses the screen's observation whenever it is under 15 s old, whatever namespace the screen fell back to. When it is older, one read is made and the screen picks that same observation up at its next refresh. There is no second classifier and no second cache to disagree with the first.

*Alternatives rejected.* Passing the TUI's in-memory report: it is nil when the screen was never opened and stale when the screen is not visible, which is when the copilot is most likely used. Reading on the UI goroutine: blocks the browser. Reading independently of the shared cache: can show one set of findings on the screen and another to the model.

*Consequence.* A preload may trigger the read that the screen would otherwise have made. It fills the shared slot, so the screen's next refresh reuses it instead of reading again; the "same findings" property is preserved, not weakened. The screen's cursor is preserved across a refresh by the existing `rebuildHealth` key matching.

### D3 — A 10-second wait and a 6 KB budget, both stated when they bite

- `agent.HealthSnapshotTimeout = 10 * time.Second` bounds the snapshot call. On error or timeout the system block says `cluster health snapshot unavailable: <reason>; call get_findings to read it now, and do not infer health from listings`.
- `agent.HealthSnapshotBytes = 6000` is the budget handed to `Report.Text`. `Report.Text` already keeps every class's state and drops findings from the end, counting them per class (K-44). Nothing new is needed in `internal/health`.
- The report's own age line survives every cut.

*Why 10 s.* `kube.HealthReadTimeout` is 10 s per class, and the reads run side by side; a cold observation normally completes inside that. The `HealthObservation` read is detached from the caller (`context.WithoutCancel`), so a caller that gives up does not cancel it: the cache is still filled for the next turn and the screen. Waiting for the full `HealthObservationBound` (40 s) before the model starts would make the copilot look hung.

*Why 6 KB.* Roughly 1.5k tokens per iteration at a typical tokenizer, with class states first. Large enough for a few dozen findings; when there are more, the snapshot says so and the model calls `get_findings`, whose own cap is `max_result_bytes` (24 KB default).

*Alternatives rejected.* No timeout (the pane can sit silent for 40 s before the first token); filling a healthy-cluster snapshot with the "not covered" essay at the cost of findings (the tool still carries it when asked); making either value configuration (no new knobs for a bounded internal detail).

### D4 — Mouse reporting is enabled and routed by pointer, not by focus

`main` starts the program with `tea.WithMouseCellMotion()`. `tui.Model.Update` handles `tea.MouseMsg` before the ordinary key routing:

- if the approval dialog is showing, mouse events are ignored (the dialog keeps its own keys);
- else if the copilot is open and `msg.X >= width - copilotWidth()`, the message is passed to `m.chat.Update` — wheel up/down moves three lines, the viewport default;
- otherwise nothing moves.

`copilotWidth()` returns the full terminal width when maximized, so the same predicate covers split and maximized without a second layout calculation.

*Why cell motion.* It reports wheel and clicks without the constant motion traffic of `WithMouseAllMotion`. `bubbles/viewport` already has wheel handling behind `MouseWheelEnabled` (default on); the missing piece is only that the program never asked the terminal to report it.

*Why pointer, not focus.* Turning the wheel over a pane is a request to scroll that pane. Requiring focus first (tab into the copilot) is the behavior the user just reported as "not scrollable".

*Trade-offs accepted.* While mouse reporting is on, a plain drag in the terminal selects nothing in most emulators; shift+drag still selects. That is the normal price of a wheel-scrollable TUI and is stated in the README. Wheel over the browser does not scroll the listing — not asked for, and the browser's `pgup`/`pgdown` already work.

### D5 — Scroll keys, a visible range, and no yanking the reader

Keys, only while the copilot pane has focus, in `onCopilotKey`:

| key | movement |
|---|---|
| `↑` `↓` | one line |
| `pgup` `pgdown` | one page (existing) |
| `ctrl+u` `ctrl+d` | half a page (existing) |
| `ctrl+home` `ctrl+end` | top / bottom |

`home` and `end` are not used: the text input already uses them to move the caret. `renderChat` records `m.chat.AtBottom()` before `SetContent` and calls `GotoBottom()` only if it was true; `ask` forces `GotoBottom()` when the user sends, so their own message is visible even if they were reading history.

*Why.* Auto-scrolling on every assistant token is what makes a scrolled-up view unusable: the reader is pulled back mid-sentence. Scrolling to the end on send matches every chat UI and is the one moment the user expects it.

The title line gains `· first–last/total` when `TotalLineCount() > Height`, computed from `YOffset`, `Height` and `TotalLineCount`, omitted when it fits or when the title has no room for it. A range is more honest than a percentage: it says exactly which visible slice of wrapped transcript the reader is on.

*Alternative rejected.* A separate scrollbar column: the transcript is already hard-wrapped to the pane, and a one-column bar costs width in the narrowest pane where space matters most.

### D6 — No keyword fast path

Every user turn gets the snapshot, not only turns whose text looks health-related.

*Why.* A classifier on the user's words ("broken", "wrong", "healthy") is wrong in both directions — "the deployment is stuck" is a health question without those words, and "is this broken label value valid?" is not one — and it would still need the snapshot as a fallback. One observation read, reused for 15 s, is cheap enough to always take.

## Risks / Trade-offs

- **Token cost on every turn** → 6 KB cap, class states first, findings cut and counted; the observation is reused for 15 s rather than read per turn.
- **A cold read delays the first token by up to 10 s** → the spinner keeps the UI alive, the read continues in the background, and the timeout is stated to the model rather than hidden. The next turn is warm.
- **Snapshot and screen can differ by one refresh** → both read the same slot; a preload's read is what the screen's next refresh adopts, and `HealthFreshFor` prevents a second read inside 15 s.
- **Mouse reporting breaks plain drag-selection** → shift+drag still selects; README states it.
- **Wheel over the transcript during an approval dialog** → ignored; the dialog is modal and its own scroll keys still work.
- **The snapshot is a picture of now** → its age is in the text and in the instructions; the model is told to call `get_findings` when the user asks for current state and the snapshot is old.
- **The position indicator counts wrapped lines, not messages** → the range describes what is visible, which is the useful unit for scrolling; it changes when the pane width changes.

## Migration Plan

None. No new configuration, no persisted state, no change to the audit format or any tool's wire behavior. Rollback is removing the `Snapshot` wiring (the block disappears) and the `WithMouseCellMotion` option (wheel scrolling stops, keys remain). The health screen and `get_findings` continue to work throughout.
