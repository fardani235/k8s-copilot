## 1. The model (`internal/metrics`, no I/O)

- [x] 1.1 `Amount` (unknown ≠ zero), `SourceStatus` with six states, `Snapshot`, and `Build` joining node metrics, pod metrics, node objects and pod objects — each side of a join optional; verify `metrics.TestUnknownIsNeverZero`, `kube.TestMetricsJoinsUsageWithBounds`
- [x] 1.2 `Query` → `Listing` for nodes, namespaces, pods (all / of a node / of a namespace) and containers, with summary gauges, heat against hard bounds only, caveat notes, sort and filter; verify `metrics.TestListings`, `TestContainers`, `TestTableText`
- [x] 1.3 A listing whose source is missing has no rows and no columns, only the status; one explanation per state; verify `metrics.TestMissingSourceIsAStateNotATable`, `TestPartialSources`
- [x] 1.4 Freshness from the sample's own timestamp, old-sample flag, hold-over marking, narrowing a cluster-wide snapshot to a namespace without changing a reading; verify `metrics.TestFreshnessAndHold`, `TestNarrowKeepsTheReadings`, `TestSnapshotIsSafeToShare` (under `-race`)

## 2. Reading the cluster (`internal/kube`)

- [x] 2.1 `Cluster.Metrics`: four `list` reads through the dynamic client, side by side, each classified on its own (absent / unavailable / denied / timed out / failed); verify `kube.TestMetricsSourceStates` and — over real HTTP, with the wire format metrics-server sends — `kube.TestMetricsOverTheWire`
- [x] 2.2 Shared cache: reuse within `maxAge`, one read for concurrent callers, a detached read a caller can stop waiting for, hold-over of the last good readings for a bounded time; verify `kube.TestMetricsAreSharedNotRefetched`, `TestMetricsHoldOverIsLabelled`, `TestHeldReadingsExpire`, `TestMetricsCallerCanStopWaiting`
- [x] 2.3 The fake cluster serves, omits (404, the default) or fails the metrics API; verify the tests above use it and `internal/guard` passes unchanged (still one write call, one raw GET, no subprocess)

## 3. The copilot (`internal/tools`, `internal/agent`)

- [x] 3.1 `get_metrics` read tool with the screen's four levels, strict arguments, row limit; verify `tools.TestMetricsTool`, `TestMetricsToolArguments`, `TestRegistryContents` (six read, four mutate)
- [x] 3.2 A missing source is an error result in the screen's words that says usage is unknown, not zero; held or old readings carry a warning; verify `tools.TestMetricsToolSaysWhenTheSourceIsMissing`, `TestMetricsToolFlagsHeldReadings`, `agent.TestMissingMetricsReachTheModelAsUnknown`
- [x] 3.3 System prompt: check load with `get_metrics`; unknown is not idle; verify `agent.TestMetricsReachTheModel`

## 4. The screen (`internal/tui`)

- [x] 4.1 `M` opens the metrics screen from any browser view; tabs, drill-down path, breadcrumb, summary gauges, `esc` back up one level landing on the row it came from; verify `tui.TestMetricsScreenDrillDown`, `TestMetricsKeyIsEverywhereAndQuitWorks`
- [x] 4.2 Sort, filter (the browser's filter input, kept separate from the listing's filter), cursor that follows the row across refreshes; verify `tui.TestMetricsSortFilterAndStickyCursor`
- [x] 4.3 Unavailable states drawn as states — not installed, not answering, not permitted — each distinct, with the server's message, and recovery; verify `tui.TestMetricsUnavailableIsSaidPlainly`
- [x] 4.4 Partial permissions: fall back to the browser's namespace and say so; pods visible but metrics refused; verify `tui.TestMetricsWithPartialPermissions`
- [x] 4.5 Self-refresh while visible at `max(refresh, 10s)`, never on top of an outstanding read, not while hidden; a failed refresh marked as not current; verify `tui.TestMetricsRefreshesItself`, `TestMetricsRefreshFailureIsMarked`, `TestMetricsSlowSourceDoesNotBlock`
- [x] 4.6 Small terminals: column drop order, gauge ladder, short freshness, and the renderer bounding its own output; verify `tui.TestMetricsOnSmallTerminals` (reachable sizes, and directly at sizes below them)
- [x] 4.7 `l` / `e` / `d` from a row, logs opening on the selected container; focus published for the copilot; cluster-originated text sanitised; verify `tui.TestMetricsOpensLogsEventsDetail`, `TestMetricsTextIsSanitized`
- [x] 4.8 Footer hints and help text for the screen; verify the strings are present (`TestMetricsKeyIsEverywhereAndQuitWorks`)

## 5. Consistency, end to end

- [x] 5.1 With the screen open, the copilot is given the snapshot on the screen (no second read even after the cluster's numbers change), and every row it is given equals the screen's, cell for cell, at three levels; verify `tui.TestCopilotSeesWhatTheScreenShows`
- [x] 5.2 With no metrics API, the copilot and the screen say the same thing; verify `tui.TestCopilotAndScreenAgreeWhenMetricsAreMissing`

## 6. Verification

- [x] 6.1 `make check` (vet, gofmt, all tests) and `make race` (now including `internal/kube` and `internal/metrics`) pass
- [x] 6.2 Mutation check: 23 deliberate breakages, each of which fails the suite — eight of the original properties (unknown drawn as `0m`, missing source drawn as a table, tool re-reading instead of sharing, 403 classified as absent, polling over an outstanding read, hold-over unmarked, a write verb in `internal/kube`, no width guarantee) and each of the fifteen review fixes in section 7 reverted in turn
- [x] 6.3 Against a real API server (minikube, Kubernetes v1.35.1, **no** metrics-server): `tools.TestLiveMetricsReadOnly` — 404 classified as "not installed", core reads succeed, the tool returns the error result; and the built binary in a real terminal — `M` shows the state, auto-retry every 10 s, 60×16 beside the copilot, below-minimum message, clean exit
- [x] 6.4 Independent adversarial review of the change; findings fixed (section 7) or recorded as limits (`docs/decisions.md` K-38)
- [ ] 6.5 **By hand — not done:** the screen and `get_metrics` against a real metrics-server. The happy path is covered by fakes and by an HTTP server speaking metrics-server's wire format, but has not been watched on a live cluster: the available cluster has no metrics-server and installing one was not this change's to do. `minikube addons enable metrics-server`, wait a minute, then `M`; compare with `kubectl top nodes` / `kubectl top pods -A`
- [ ] 6.6 **By hand — not done:** ask the copilot, with a real model, why a workload is slow, with the metrics screen open; check it calls `get_metrics` and quotes what is on the screen

## 7. Fixes from the independent review

- [x] 7.1 Replace the per-scope cache with one cluster-wide read that every answer is cut from; a namespace's pods read separately only when the cluster-wide read is refused, and joined to the same node readings; verify `kube.TestMetricsAreCoherentAcrossScopes`, `metrics.TestWithPodsOf`
- [x] 7.2 The screen takes up the readings the copilot's tool had to read; verify `tui.TestScreenAdoptsWhatTheCopilotRead`
- [x] 7.3 Running init and ephemeral containers keep their declared bounds; a container not in the spec is unknown, not "none"; pod bounds follow the containers that are running; verify `metrics.TestRunningInitContainerKeepsItsBounds`
- [x] 7.4 A sample without containers is no reading; verify `metrics.TestSampleWithoutContainersIsNotZero`
- [x] 7.5 CPU in nanocores, rounded for display only; absurd quantities unknown; percentages cannot overflow; verify `metrics.TestSmallReadingsAreNotRoundedUpBeforeAdding`, `TestAbsurdQuantitiesAreUnknown`
- [x] 7.6 Pod heat from its containers; no cluster share over partly known capacity; metrics-only pods on an unknown node; verify `metrics.TestPodIsAsHotAsItsHottestContainer`, `TestClusterShareNeedsEveryReportingNodesCapacity`, `TestPodKnownOnlyToTheMetricsAPI`
- [x] 7.7 A namespace that was not read is not called empty; the screen leaves a drill-down it can no longer show and widens again when a refusal is lifted; verify `metrics.TestNamespaceOutsideWhatWasRead`, `tui.TestMetricsDoesNotCallAnUnreadNamespaceEmpty`
- [x] 7.8 The sample's age is the older source's and keeps counting on an untouched screen; verify `metrics.TestSampleTimeIsTheOlderSource`, `tui.TestMetricsAgeKeepsCounting`, and the binary with `--refresh off`
- [x] 7.9 Filter first on its line; caveats use the room the rows leave and are drawn under states too; the selected row keeps its heat colour; verify `tui.TestMetricsFilterIsVisibleInANarrowPane`, `TestMetricsCaveatsUseTheRoomThereIs`, `TestSelectedRowKeepsItsHeat`
- [x] 7.10 Tool result: caveats before rows, rows dropped and counted to fit the cap, names validated, cells cleaned; verify `tools.TestMetricsToolKeepsItsCaveatsWhenShortened`, `TestMetricsToolArguments`, `metrics.TestTableTextCannotBeForged`
- [x] 7.11 401 is its own state; 429 is "failed"; paging cannot loop; verify `kube.TestMetricsSourceStates`, `tools.TestMetricsToolExpiredCredentials`, `tui.TestMetricsExpiredCredentialsAreSaidToBeThat`, `kube.TestMetricsPagingCannotLoop`
- [x] 7.12 `M` returns to an open metrics screen instead of stacking another; verify `tui.TestMetricsKeyReturnsToTheOpenScreen`
- [x] 7.13 Rewrite the four tests that passed without proving their comments

## 8. Documentation

- [x] 8.1 README (keys, the screen, what the copilot can read), `docs/architecture.md`, `docs/decisions.md` (K-25 – K-38), `docs/security.md`, `docs/testing.md`, `docs/configuration.md`, CHANGELOG
