# Tasks

## 1. Health snapshot in the agent's turn context

- [x] 1.1 Add `HealthSnapshotBytes` (6000), `HealthSnapshotTimeout` (10 s) and `Config.Snapshot func(context.Context) (string, error)`; fetch once per `Run`, append a labelled block to the system context (never to `history`), and on error or timeout append a stated "snapshot unavailable" note and continue the turn; verify `agent.TestHealthSnapshotAppearsInEveryTurn` (present in every `Request.System`, called once across several iterations, absent from the stored user message) and `agent.TestHealthSnapshotFailureIsStated`
- [x] 1.2 Extend the system prompt: answer "what is broken?", "what is wrong?" and "is the cluster healthy?" from the attached snapshot without listing resources; call `get_findings` when the snapshot reports omitted findings, is marked unavailable, or the user asks for the current state and the snapshot is old; keep the existing rule that an unwatched class is never healthy; verify `agent.TestSystemPromptHealthSnapshotRules`
- [x] 1.3 Record the decision (K-46) in `docs/decisions.md` naming the rejected alternatives (a synthetic user message and a tool result) and add the snapshot to the request-context description in `docs/architecture.md`; add the CHANGELOG entry; verify the decision names both alternatives and the architecture text names `HealthObservation` as the snapshot's source

## 2. Wire the snapshot to the shared health observation

- [x] 2.1 In `cmd/k8s-copilot/main.go`, build the agent's `Snapshot` from `cluster.HealthObservation(ctx, kube.AnyFallback, kube.HealthFreshFor)`, `health.Observe`, and `Report.Text(time.Now(), agent.HealthSnapshotBytes)`; verify `go build ./...` and `go vet ./...`, and that no new configuration key or flag is added (`go test ./internal/config`)
- [x] 2.2 Confirm the snapshot path is read-only: `HealthObservation` only sends typed `list` requests, `internal/guard` is untouched and still passes; verify `go test ./internal/guard` and inspect the diff for write calls

## 3. Scrollable copilot pane

- [x] 3.1 Start the program with `tea.WithMouseCellMotion()` and handle `tea.MouseMsg` in `Model.update`: ignore it while the approval dialog is visible, otherwise pass wheel events to the transcript when the pointer is over the copilot pane (split and maximized), and do nothing over the browser or when the pane is hidden; verify `tui.TestCopilotWheelScrollsOnlyOverThePane`
- [x] 3.2 Add `↑`/`↓` (one line) and `ctrl+home`/`ctrl+end` (ends) to `onCopilotKey`, keep `pgup`/`pgdown`, `ctrl+u`/`ctrl+d`, and leave `home`/`end` to the input's caret; extend the test key helper for these names; verify `tui.TestCopilotScrollKeys`
- [x] 3.3 Make `renderChat` auto-scroll only when the reader was already at the bottom, and force the bottom when the user sends a message; verify `tui.TestCopilotScrollIsNotYankedByNewOutput` and `tui.TestSendingReturnsToTheEnd`
- [x] 3.4 Show `first–last/total` on the copilot title line whenever the transcript is longer than the pane, update it as the view moves, and omit it when it fits or when the title has no room; verify `tui.TestCopilotScrollRangeIndicator` and that the pane still fits at 60×16 (`tui.TestCopilotPaneFitsNarrowestTerminal`)
- [x] 3.5 Document the keys and mouse behavior in README (including shift-drag for terminal selection), the `?` help text where copilot keys are listed, the decision (K-47) in `docs/decisions.md` naming the rejected alternatives (focus-routed wheel, a scrollbar column), and the CHANGELOG; verify the README key table matches the implemented bindings and the decision names both alternatives

## 4. Integration and hand-over

- [x] 4.1 Run `make check` and `make race`; both pass with the new tests
- [x] 4.2 Mutation check: remove the snapshot from the system context, always auto-scroll to the bottom, and route the wheel regardless of pointer; confirm each revert fails its test
- [ ] 4.3 **By hand** in a real terminal: on a cluster with a known finding, ask "what is broken?" and confirm the answer names that finding with no `list_resources` discovery pass (the transcript's tool lines show `get_findings` at most); confirm wheel and key scrolling, the range indicator, that new output does not pull a scrolled-up view to the bottom, and that shift-drag still selects text. Record the result and any terminal-specific notes in `docs/testing.md`
- [x] 4.4 Independent review of the diff against the two specs: the snapshot is never a second classification, an unavailable snapshot is never silent, mouse events cannot move another pane or a modal, and scroll state survives resize and maximize/restore; fix or record the findings

## Workflow follow-up

- Archive the change after the review and the by-hand check are done.
- Verify `openspec validate --all --strict` after archive.
