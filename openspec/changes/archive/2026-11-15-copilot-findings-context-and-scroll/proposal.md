# Proposal

## Why

The health screen and the `get_findings` tool already classify the cluster once, but the copilot only sees that report if the model decides to call the tool. A model that does not call it answers "what is broken?" by listing pods, nodes, workloads and events itself — the default 30-tool-call budget is exactly the scale of that discovery pass, and it can exhaust the request before producing an answer.

Separately, the copilot transcript is a viewport that only responds to `pgup`, `pgdown`, `ctrl+u` and `ctrl+d`, and only while the pane has focus. The mouse wheel does nothing because the program never enables mouse events; arrow keys do nothing; and there is no indication of where the reader is in a long answer. Long explanations therefore look like they are not scrollable at all.

## What Changes

- Attach a **bounded cluster health snapshot** to every copilot turn. It is rendered from the same `health.Report` the health screen draws, from the same shared observation, and carries the reading's age, every class's state, and as many findings as fit. The model is told to answer health questions from it without a discovery pass, and that `get_findings` remains the way to refresh or see findings the snapshot had to omit.
- If the snapshot cannot be taken (read error or timeout), the turn says so explicitly; the model is never left to infer health from listings, and the browser and the rest of the conversation continue.
- Make the **copilot pane scrollable**: enable mouse wheel routing to the transcript when the pointer is over the pane, add `↑`/`↓` line scrolling and `ctrl+home`/`ctrl+end`, keep `pgup`/`pgdown` and `ctrl+u`/`ctrl+d`, and show the visible line range when the transcript is longer than the pane.
- Stop new output from forcing the transcript back to the bottom while the user is reading older content; sending a message still returns the view to the end.
- No new configuration, cluster-side install, permission or write path.

Not in this change: changing the health classification or its thresholds; adding new health classes; persisting findings; turning the snapshot into a tool the model must call; mouse support for the browser listing or sub-view panes; text selection changes beyond the normal terminal shift-click behavior when mouse reporting is on.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `agent-copilot`: the context provided to the model at the start of every turn gains the current classified findings, with explicit rules for when the snapshot is partial, stale or unavailable.
- `resource-browser`: the copilot pane gains mouse-wheel and keyboard scrolling, a visible position indicator, and a rule that reading older content is not interrupted by new output.

## Impact

- **`internal/agent`**: a `FindingsSnapshot` source in `agent.Config`, a bounded fetch per request, a new block in the request's system context, and system-prompt rules for using it. Conversation history is not given a copy on every turn.
- **`internal/tui`**: mouse-wheel routing for the copilot pane, additional transcript keys, a scroll-position indicator on the pane's title line, and auto-scroll only when the reader is already at the end.
- **`cmd/k8s-copilot`**: enable mouse cell-motion reporting and wire the snapshot source to the cluster's shared health observation.
- **`internal/health`**: unchanged classification; the snapshot is `Report.Text` under a smaller budget, so every class state survives while findings may be dropped and counted.
- **Unchanged on purpose**: `internal/guard`, the mutation set, the approval gate, the audit format and `go.mod`. The snapshot is a read-only reuse of the observation the health screen already reads.
- **Docs**: README (copilot keys and the health answer), `docs/architecture.md`, `docs/decisions.md`, `docs/testing.md`, CHANGELOG.
