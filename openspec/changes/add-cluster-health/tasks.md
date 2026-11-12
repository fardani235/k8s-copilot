# Tasks

## 1. The health model (`internal/health`, pure)

- [ ] 1.1 `Finding`, `Class`, `ClassState` (`watched`, `forbidden`, `unavailable`, `not applicable`), `Report`, and the sentences for each state; verify `health.TestStatesAreDistinct` fails if any two states render the same text
- [ ] 1.2 Workload classification — crash-looping/restarting threshold, image-pull failure, running-but-not-ready with the stated grace; verify `health.TestWorkloadFindings`
- [ ] 1.3 Node classification — NotReady, memory pressure, disk pressure, one finding per condition; verify `health.TestNodeFindings`
- [ ] 1.4 Rollout classification for Deployments, StatefulSets and DaemonSets; a paused or completed rollout produces nothing; verify `health.TestRolloutFindings`
- [ ] 1.5 Warning events: the stated window, newest first, identical warnings grouped with a count; verify `health.TestWarningEventWindowAndGrouping`
- [ ] 1.6 Usage findings call the existing `metrics.HeatOf` threshold rather than a second definition; no metrics snapshot means the class is not applicable and produces no finding; verify `health.TestUsageUsesTheSharedHeatThreshold`, `health.TestNoMetricsIsNotClear`
- [ ] 1.7 Per-class availability from read outcomes, including truncation and namespace fallback: a class that was not read contributes no finding and never renders as watched-and-clear; verify `health.TestUnreadClassIsNeverClear`, `health.TestTruncatedReadSaysSo`
- [ ] 1.8 Every name, reason and message passes `textutil.Sanitize` and is bounded in length; verify `health.TestFindingTextIsSanitizedAndBounded`
- [ ] 1.9 Record the model decisions (D1–D3, D5) in `docs/decisions.md` as K-39…, and verify each entry names the alternative it rejected
- [ ] 1.10 Add `./internal/health/` to the `race` target in the `Makefile`; verify `make race` runs the new package

## 2. Reads the monitor needs (`internal/kube`)

- [ ] 2.1 Typed, read-only listing helpers for pods, nodes, Deployments, StatefulSets and DaemonSets, each returning its own error and the number of rows cut by the 500-row cap; verify `kube.TestTypedHealthListings` and `kube.TestTruncationIsReported`
- [ ] 2.2 Cluster-wide first, browser-namespace fallback when the cluster-wide list is refused, with the scope reported to the model; verify `kube.TestHealthListingsFallBackToNamespace`
- [ ] 2.3 Warning-events helper bounded by the stated window and a row cap; verify `kube.TestWarningEventsWindowAndCap`
- [ ] 2.4 Assemble one observation reusing the existing metrics cache — no second read of the metrics API — and pass each read's status through unchanged; verify `kube.TestObservationReusesMetricsSnapshot`
- [ ] 2.5 Confirm `internal/guard` is untouched and still passes: one write call, one raw GET, one approval call site, no subprocess; verify `go test ./internal/guard`
- [ ] 2.6 Record D2 and D6 (attempt-and-classify; typed reads and the truncation rule) in `docs/decisions.md` and add the read path to `docs/architecture.md`; verify the architecture diagram names `internal/health` and the four reads

## 3. The copilot's view (`internal/tools`, `internal/agent`)

- [ ] 3.1 `get_findings` as a read tool over the same `health.Report`, capped at `MaxResultBytes`, cutting findings but never class states; verify `tools.TestGetFindings` and `tools.TestFindingsCapKeepsClassStates`
- [ ] 3.2 Update the registry count test — seven read tools, four mutate tools — and confirm the unknown-tool refusal text still lists the read set; verify `tools.TestRegistry`
- [ ] 3.3 Two system-prompt sentences: check findings with the tool instead of inferring health, and never call an unread class healthy; verify `agent.TestSystemPromptExplainsFindings`
- [ ] 3.4 Record D9 in `docs/decisions.md` and describe the tool in `docs/architecture.md`; verify the tool appears in `k8s-copilot config`-adjacent docs where the other tools are listed

## 4. The screen (`internal/tui`)

- [ ] 4.1 The health view: one key from any browser view, findings grouped by class, class state line, age of the reading, key hints for that screen; verify `tui.TestHealthKeyOpensAndReturnsWithBrowserState`
- [ ] 4.2 Shared filter over object, namespace, class and reason, and the cursor follows the finding across a refresh; verify `tui.TestHealthFilterAndStickySelection`
- [ ] 4.3 `enter`, `l`, `e` open detail, logs and events for the selected finding's object and return to the same finding; verify `tui.TestHealthOpensSubjectViews`
- [ ] 4.4 Refresh tick at `max(refresh_interval, 15s)` while visible, none while not visible, no overlap, per-class timeout with a stated reason; verify `tui.TestHealthRefreshesOnlyWhileVisible` and `tui.TestHealthSlowReadDoesNotBlock`
- [ ] 4.5 Focus text for the health screen — screen, classes, selected finding — and the browser focus restored on leaving; verify `tui.TestHealthFocusText`
- [ ] 4.6 Rendering stays inside the pane at 60×16 and narrower, keeps each finding's object and class, cuts text with a marker; verify `tui.TestHealthFitsNarrowPane`
- [ ] 4.7 README (the health screen, its keys, what it does not cover), `helpText()`, `docs/testing.md` (the by-hand checks) and CHANGELOG; verify the help text lists the key and the README section matches the screen

## 5. Integration and review

- [ ] 5.1 `make check` and `make race` pass, including `internal/health`, and `go test ./internal/guard` passes unchanged; verify by running all three
- [ ] 5.2 Mutation check: revert each honesty rule in turn — an unread class rendered clear, a truncated read rendered clear, absent metrics rendered "nothing hot", the tool dropping class states when it cuts — and confirm each revert fails the suite
- [ ] 5.3 **By hand** on this box against minikube: force a pod into CrashLoopBackOff and a Deployment into a stalled rollout, open the screen, confirm two findings, open logs/detail from each, filter, leave and return with browser state intact; with a namespace-only credential confirm the affected classes say they are namespace-scoped
- [ ] 5.4 **By hand, needs metrics-server**: confirm a hot node or container produces one finding matching the metrics screen's red row, then remove metrics-server and confirm the class becomes not applicable rather than clear
- [ ] 5.5 Independent adversarial review of the change; findings fixed or recorded as limits in `docs/decisions.md`

## Workflow follow-up

- Archive the change once the review and the by-hand checks are done, and confirm the archived `cluster-health` spec keeps its Purpose.
- Verify `openspec validate --all --strict` after archive.
