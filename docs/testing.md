# Testing

```sh
make test     # go test ./...        — no cluster and no model needed
make vet      # go vet + gofmt check
make race     # the concurrent packages under -race
make live     # opt-in: read-only + dry-run against your CURRENT kube context
              # (-run LiveMetrics alone sends list requests only)
```

## How it is tested without a cluster or a model

- **Cluster**: `internal/kube/kubetest` builds a `kube.Cluster` on client-go's
  fake dynamic and typed clients, with a fake discovery that can fail. It can
  also serve the metrics API — or, by default, not serve it (404, like a
  cluster without metrics-server), or fail it with any error. The
  stock fakes persist a patch even when it is a dry-run, so the fake intercepts
  every patch: it records it, lets a test inject a rejection (Forbidden,
  Conflict, admission), and for a dry-run returns the object unchanged. That is
  what lets the tests assert "a dry-run changed nothing" and "the applied
  request is byte-identical to the validated one".
- **Model**: `llm.Stub` plays a script of tool calls and messages, so the loop,
  the gate and the audit trail are driven deterministically.
- **Provider adapters**: `httptest` servers check the exact wire format of
  both protocols and the error paths.
- **UI**: the Bubble Tea model is driven synchronously — key presses in,
  rendered screen out — including the full copilot → proposal → dialog →
  decision → apply → audit path, with a real agent, the stub model and the fake
  cluster.
- **Structure**: `internal/guard` parses the source and fails if a safety
  property stops holding (see docs/security.md).

## Task → test map

| Task | Verified by |
|---|---|
| 1.1 skeleton | `go build ./...`; `cmd/k8s-copilot` tests. Terminal restore: by hand (below) |
| 1.2 kubeconfig resolution | `kube.TestLoadConfigResolution` (KUBECONFIG precedence, default path, override, unknown context) |
| 1.3 connection errors | `kube.TestLoadConfigMissing`, `kube.TestConnectFailures`, `main.TestStartupFailuresExitNonZero` (messages + exit code) |
| 2.1 discovery | `kube.TestTypesDiscovery`, `TestResolve`, `TestTypesDiscoveryFailure`, `tui.TestTypePicker`, `TestDiscoveryFailureIsShown` |
| 2.2 namespaces | `kube.TestListScoping`, `tui.TestNamespacePicker` |
| 2.3 table columns | `kube.TestListScoping` (fallback), `kube.TestListServerTable` (server Table), `tui.TestNamespacePicker` (namespace on every row) |
| 2.4 empty / forbidden | `kube.TestListEmptyAndForbidden`, `tui.TestEmptyAndForbiddenStates` |
| 2.5 detail round trip | `tui.TestDetailRoundTrip`, `kube.TestSummarizeAndYAML` |
| 2.6 navigation, quit, too small | `tui.TestTooSmallAndQuit` (quit key from every view). Terminal restore: by hand |
| 3.1 logs | `kube.TestLogs`, `tui.TestLogsView`, `TestLogBufferIsBounded` |
| 3.2 events | `kube.TestEventsOrderAndFilter`, `tui.TestEventsView` |
| 4.1 copilot pane | `tui.TestCopilotToggleKeepsBrowserState` |
| 4.2 provider interface + stub | `llm.TestStubIsDeterministic`, `agent.TestReadToolAutoExecutes` |
| 4.3 loop, bounds | `agent.TestIterationLimit`, `TestToolCallLimit`, `TestBusy` |
| 4.4 focus injection | `agent.TestFocusIsInjectedEachRequest`, `tui.TestAskUsesFocusAndShowsAnswer`, `TestFocusStoreSeenByAgentAfterChange` |
| 4.5 adapter, credentials, failure | `llm.TestAnthropicAdapter`, `TestOpenAICompatibleAdapter`, `TestProviderErrors`, `TestNewValidatesConfiguration`, `config.TestProviderCredential`, `agent.TestProviderFailure`, `tui.TestProviderFailureLeavesBrowserUsable` |
| 5.1 registry | `tools.TestRegistryContents`, `TestUnknownAndMalformedCalls`, `TestTiersCannotBeCrossed` |
| 5.2 read tools | `tools.TestReadTools`, `TestReadToolSurfacesForbidden`, `TestReadResultIsCapped`, `agent.TestForbiddenReadReachesModel` |
| 5.3 mutate tools, refusals | `tools.TestMutationPatchShapes`, `TestUnsupportedMutationsAreRefused`, `TestNoOpAndGuards`, `agent.TestUnknownToolGoesBackAsError` |
| 5.4 dry-run before the gate | `agent.TestDryRunRejected`, `tools.TestDryRunChangesNothingAndGrantsAreBound` |
| 5.5 no shelling out | `guard.TestNothingCanRunASubprocess` (+ the other guard tests) |
| 6.1 dialog content | `tui.TestApprovalDialogContentScale`, `TestApprovalDialogContentLabels` |
| 6.2 blocking gate | `agent.TestUnansweredProposalNeverApplies`, `TestRejected`, `tools.TestGateBlocksUntilDecision`, `tui.TestHideKeepsWaiting`, `TestApproveFlow` (incl. "ordinary typing cannot answer"), `TestRejectFlow`, `TestQuitWhileWaiting`, `TestDialogScrollsWithoutHidingContent` |
| 6.3 apply failure | `agent.TestApplyFailsAfterApproval` (conflict, RBAC, admission), `tools.TestApplyRefusesWhenStateMoved` |
| 6.4 end to end | `agent.TestEndToEndApproved`, `TestRejected`, `TestDryRunRejected`, `TestEditRevalidatesAndAsksAgain`, `tui.TestApproveFlow`, `TestEditFlow` |
| 7.1 audit entries | the `agent` tests above assert the entry for approved / rejected / dry-run-rejected / failed / edited / cancelled |
| 7.2 hash chain | `audit.TestAppendChainsAndVerifies`, `TestTamperingIsDetected`, `TestTruncationWithHeadRemoved`, `TestTwoWritersKeepOneChain`, `guard.TestAuditIsAppendOnly`, `main.TestAuditCommands` |
| 7.3 unwritable, restart | `audit.TestUnwritable`, `TestPersistsAcrossRestart`, `TestAppendAfterTornWrite`, `TestReplacedFileIsDetected`, `agent.TestNoAuditNoMutation`, `TestApprovedButUnrecordableIsNotApplied` |
| 8.1 configuration | `config.TestDefaults`, `TestPrecedence`, `TestRejectsBadConfiguration`, `TestSubcommands` |
| 8.2 suite | `go test ./...` and `go vet ./...` pass; `-race` clean on agent, tui, audit, tools |
| 8.3 manual smoke | **partly — see below** |
| 8.4 refusals in the running binary | automated equivalent passes (`tools.TestUnsupportedMutationsAreRefused`, `agent.TestUnknownToolGoesBackAsError`); **by hand: pending** |

## The metrics screen (`add-metrics-dashboard`)

What has to be true, and what shows it:

| Property | Verified by |
|---|---|
| A missing reading is `—`, a measured zero is `0`, "not set" is neither | `metrics.TestUnknownIsNeverZero`, `kube.TestMetricsJoinsUsageWithBounds` |
| A missing source is a stated reason, never a table | `metrics.TestMissingSourceIsAStateNotATable`, `tui.TestMetricsUnavailableIsSaidPlainly`, `tools.TestMetricsToolSaysWhenTheSourceIsMissing` |
| Not installed / not answering / not permitted / too slow are told apart | `kube.TestMetricsSourceStates`, `kube.TestMetricsOverTheWire` (real HTTP: 404, plain-text 503, 403, a hang) |
| One source missing leaves the others working, with the caveat stated | `metrics.TestPartialSources`, `tui.TestMetricsWithPartialPermissions` |
| Read-only: `list` and nothing else | `kube.TestMetricsJoinsUsageWithBounds` (no mutating action), `kube.TestMetricsOverTheWire` (every request is a GET), `guard.TestSingleWritePath` (unchanged) |
| Units are read correctly (nanocores, Ki) | `kube.TestMetricsOverTheWire` — metrics-server's wire format, through the real client |
| Node → pods → containers, namespace → pods → containers, and back to the same row | `tui.TestMetricsScreenDrillDown` |
| Sort, filter, a cursor that follows the row through a refresh | `tui.TestMetricsSortFilterAndStickyCursor` |
| The copilot is given the snapshot on the screen, and the same cells | `tui.TestCopilotSeesWhatTheScreenShows`, `kube.TestMetricsAreSharedNotRefetched`, `metrics.TestNarrowKeepsTheReadings` |
| Copilot and screen say the same when there are no metrics | `tui.TestCopilotAndScreenAgreeWhenMetricsAreMissing`, `agent.TestMissingMetricsReachTheModelAsUnknown` |
| Refreshes itself while visible, at the source's pace, never over an outstanding read | `tui.TestMetricsRefreshesItself` |
| A failed refresh keeps the last readings, marked, for a bounded time | `kube.TestMetricsHoldOverIsLabelled`, `kube.TestHeldReadingsExpire`, `tui.TestMetricsRefreshFailureIsMarked`, `tools.TestMetricsToolFlagsHeldReadings` |
| A hung metrics API does not hang the interface or other requests | `tui.TestMetricsSlowSourceDoesNotBlock`, `kube.TestMetricsOverTheWire/slow`, `kube.TestMetricsCallerCanStopWaiting` |
| Small terminals: nothing drawn outside the pane, essentials kept | `tui.TestMetricsOnSmallTerminals` (reachable sizes, and the renderer directly below them), `tui.TestUnavailableCopilotKeepsItsFrame` |
| Names and server messages cannot drive the terminal | `tui.TestMetricsTextIsSanitized` |
| A snapshot shared between the UI and the agent is never written to | `metrics.TestSnapshotIsSafeToShare` under `-race` |
| Whole cluster or one namespace, in any order: the same readings | `kube.TestMetricsAreCoherentAcrossScopes` |
| When the copilot had to read for itself, the screen takes up what it read | `tui.TestScreenAdoptsWhatTheCopilotRead` |
| A running init container keeps its own limit; a container not in the spec is `—`, not "none" | `metrics.TestRunningInitContainerKeepsItsBounds` |
| Nothing measured is never `0`; small readings are summed before rounding | `metrics.TestSampleWithoutContainersIsNotZero`, `TestSmallReadingsAreNotRoundedUpBeforeAdding`, `TestAbsurdQuantitiesAreUnknown` |
| A pod is as hot as its hottest container | `metrics.TestPodIsAsHotAsItsHottestContainer` |
| The sample's age is the older source's and keeps counting | `metrics.TestSampleTimeIsTheOlderSource`, `tui.TestMetricsAgeKeepsCounting` |
| An unread namespace is not called empty | `metrics.TestNamespaceOutsideWhatWasRead`, `tui.TestMetricsDoesNotCallAnUnreadNamespaceEmpty` |
| Expired credentials are not described as a missing permission | `kube.TestMetricsSourceStates`, `tui.TestMetricsExpiredCredentialsAreSaidToBeThat`, `tools.TestMetricsToolExpiredCredentials` |
| A long tool result loses rows, never its caveats; a name cannot forge a row | `tools.TestMetricsToolKeepsItsCaveatsWhenShortened`, `metrics.TestTableTextCannotBeForged` |
| The filter shows in the narrowest pane; the selected row keeps its colour | `tui.TestMetricsFilterIsVisibleInANarrowPane`, `tui.TestSelectedRowKeepsItsHeat` |

**The tests were checked against themselves.** Twenty-three things were
deliberately broken, one at a time, and each made the suite fail: eight of the
original properties — an unknown reading formatted as `0m`; a listing built
without its source; the tool re-reading instead of sharing; a 403 classified
as "not installed"; polling over an outstanding read; a held snapshot left
unmarked; a `Delete` call added to `internal/kube`; the width guarantee
removed — and then each of the fifteen review fixes, reverted in turn. One
mutation (rows not clipped) survived at first because nothing reachable
exercised it; the test that now covers it also found that the renderer did
not bound its own height.

**An independent adversarial review** (decisions K-38) then found what the
author's tests had not: chiefly, three ways the copilot and the screen could
be handed different readings of the same pod. It reproduced each finding with
a throwaway test; each fix has a regression test (the last eleven rows of the
table above), and four tests it showed to be weaker than their comments were
rewritten. It also reported what held up under attack — see K-38.

**Against a real API server** (minikube, Kubernetes v1.35.1, which has *no*
metrics-server), on 2026-10-09:

- `K8S_COPILOT_LIVE=1 go test ./internal/tools -run LiveMetrics -v` — the 404
  was classified as "not installed"; the node and pod reads (served from the
  API server's cache) succeeded; `get_metrics` returned its error result
  ("Usage is UNKNOWN, not zero…") at all three levels tried. This test sends
  `list` requests only.
- The built binary in a real terminal (tmux, 120×32): `M` showed "Metrics are
  not available on this cluster" with the server's message; the screen retried
  by itself every 10 s; at 60×16 beside the copilot both panes kept their
  frames; below the minimum the usual "terminal too small" message appeared;
  `q` exited with status 0 and a clean terminal. Run again after the review
  fixes with `--refresh off`: left untouched, the title went from "read 0s
  ago" to "read 10s ago".

That run is also what found the copilot pane overflow recorded in K-37.

## The health screen (`add-cluster-health`)

| Property | Verified by |
|---|---|
| Every class state reads differently; "watched, no findings" is never "not watched" | `health.TestStatesAreDistinct` |
| Workload, node, rollout, event and usage classification, with the stated thresholds | `health.TestWorkloadFindings`, `TestNodeFindings`, `TestRolloutFindings`, `TestWarningEventWindowAndGrouping`, `TestUsageUsesTheSharedHeatThreshold` |
| No metrics is not "nothing hot"; a held snapshot is not current | `health.TestNoMetricsIsNotClear` |
| An unread, refused or partly read class contributes nothing and is never clear | `health.TestUnreadClassIsNeverClear`, `tui.TestHealthShowsWhatIsNotWatched` |
| A read cut at 500 rows, or narrowed to a namespace, says so and is not clear | `health.TestTruncatedReadSaysSo`, `kube.TestTruncationIsReported`, `kube.TestHealthListingsFallBackToNamespace` |
| Names and messages cannot drive the terminal or forge a line for the model | `health.TestFindingTextIsSanitizedAndBounded`, `tui.TestHealthFitsNarrowPane` |
| Typed, `list`-only reads, each with its own status | `kube.TestTypedHealthListings`, `kube.TestWarningEventsWindowAndCap`, `guard` (unchanged) |
| Usage comes from the metrics cache, never a second read | `kube.TestObservationReusesMetricsSnapshot` |
| One observation for concurrent callers; a caller that gives up is not told "clear" | `kube.TestHealthObservationSharesOneRead`, `kube.TestHealthCallerCanStopWaiting` |
| A slow read is abandoned after its bound, with a reason, without holding the others | `kube.TestSlowHealthReadIsAbandonedWithAReason`, `tui.TestHealthSlowReadDoesNotBlock` |
| The tool prints the model's report, cuts findings and never class states | `tools.TestGetFindings`, `tools.TestGetFindingsAllClear`, `tools.TestFindingsCapKeepsClassStates`, `health.TestTextCutKeepsClassStates` |
| Seven read tools, four mutate; the refusal lists them | `tools.TestRegistryContents` |
| The model is told to use it and not to call an unread class healthy | `agent.TestSystemPromptExplainsFindings` |
| `H` opens it from any view; leaving restores namespace, type, selection, filter | `tui.TestHealthKeyOpensAndReturnsWithBrowserState` |
| Filter over object/namespace/class/reason; the cursor follows its finding | `tui.TestHealthFilterAndStickySelection` |
| Detail, logs, events of a finding, and back to the same finding | `tui.TestHealthOpensSubjectViews` |
| Refresh only while visible, at `max(refresh_interval, 15s)`, never overlapping | `tui.TestHealthRefreshesOnlyWhileVisible` |
| The focus names the screen, its classes and the finding; restored on leaving | `tui.TestHealthFocusText` |
| The copilot's reading is what the screen shows, without a further read | `tui.TestHealthShowsWhatTheCopilotWasGiven` |
| Inside the pane at every size; class and object kept; unwatched classes first when narrow | `tui.TestHealthFitsNarrowPane` |
| Pods of one namespace only: rollouts not clear (review) | `health.TestRolloutsWithPodsOfOneNamespaceAreNotClear` |
| A metrics API with no readings is not "nothing hot" (review) | `health.TestUsageWithoutReadingsIsNotClear` |
| The tool's cut is fast and within budget, states kept (review) | `health.TestTextCutIsNotQuadratic`, `TestTextStaysWithinBudgetWithLongReasons` |
| Stuck waiting containers, nodes with no Ready condition (review) | `health.TestStuckWaitingAndMissingReadyAreFindings` |
| The copilot is handed the screen's observation, and the focus names the screen's fallback (review) | `kube.TestAnyFallbackTakesTheScreensObservation`, `tui.TestHealthFocusText` |
| The help and the README's sample match the screen | `tui.TestHealthHelpAndReadmeMatchTheScreen` |

**The tests were checked against themselves.** Each honesty rule was
reverted in turn and the suite run (`health`, `kube`, `tools`, `tui`,
`agent`): an unread class rendered clear (`Class.Clear` ignoring the state —
6 tests failed); a class with no answering read made *watched* (5); a
truncated read rendered clear (3); the truncation dropped on its way out of
`kube` (1); absent metrics rendered as a watched, quiet usage class (6+); no
snapshot rendered as watched (2); the tool dropping class states when it cuts
(2); the tool using the generic byte cap (1). One mutation survived alone:
removing `Observe`'s backstop that discards an unwatched class's findings,
because every classifier already returns nothing for a read that did not
answer — removing the backstop together with one classifier's own guard is
caught (`health.TestUnreadClassIsNeverClear`).

**Against a real API server** (this box's minikube, Kubernetes v1.35.1, with
metrics-server running), on 2026-11-13:
`K8S_COPILOT_LIVE=1 go test ./internal/tools -run LiveFindings -v` — all six
typed reads answered cluster-wide (24 pods, 1 node, 12 Deployments, 3
StatefulSets, 1 DaemonSet, 2 Warning events — the server honoured the
`type=Warning` field selector), the usage class was *watched* from the real
metrics API, and `get_findings` reported metrics-server's own readiness-probe
warnings from its start-up, grouped and newest first. `list` requests only.
The interactive checks (tasks 5.3, 5.4 — forcing a crash loop and a stalled
rollout, a namespace-only credential, a container at its memory limit, and
removing metrics-server) change the cluster and are left for a human: see
items 9 and 10 below.

## Copilot findings context and scroll (`copilot-findings-context-and-scroll`)

| Property | Verified by |
|---|---|
| The snapshot is in the system context of every model call of a request, taken once, bounded in time, never stored in the history | `agent.TestHealthSnapshotAppearsInEveryTurn` |
| A failed, timed-out or stuck snapshot is stated as unavailable, with its reason, and the request goes ahead | `agent.TestHealthSnapshotFailureIsStated` |
| The prompt says to answer health questions from the snapshot, when to call `get_findings`, and keeps the unwatched rule | `agent.TestSystemPromptHealthSnapshotRules`, `agent.TestSystemPromptExplainsFindings` |
| The snapshot is the health report from the shared observation, within 6 KB, `list` only; a caller that gave up gets an error, not a report | `main.TestHealthSnapshotIsTheScreensReport`, `guard` (unchanged) |
| The wheel scrolls the transcript only with the pointer over the pane, split or maximized, focus or not; never over the browser, header, footer, a hidden pane or the approval dialog | `tui.TestCopilotWheelScrollsOnlyOverThePane` |
| `↑ ↓`, `pgup pgdown`, `ctrl+u ctrl+d`, `ctrl+home ctrl+end` move the transcript; `home`/`end` still move the caret | `tui.TestCopilotScrollKeys` |
| New output follows a reader at the end and leaves a scrolled-up one where they are | `tui.TestCopilotScrollIsNotYankedByNewOutput` |
| Sending returns the view to the end | `tui.TestSendingReturnsToTheEnd` |
| The title's `first–last/total` is right, follows the view, is absent when it fits or has no room | `tui.TestCopilotScrollRangeIndicator` |
| The pane, range included, fits 60×16, split and maximized | `tui.TestCopilotPaneFitsNarrowestTerminal` |
| The reader's position survives resize and maximize/restore | `tui.TestCopilotScrollSurvivesResizeAndMaximize` |
| The snapshot reads with the screen's fallback namespace; the screen reuses that read (review) | `main.TestSnapshotReadsTheScreensNamespace`, `main.TestSnapshotFallbackIsTheBrowsersNamespace` |
| An open health screen takes up the snapshot's read; the pane says it is reading (review) | `tui.TestHealthScreenAdoptsTheSnapshotsRead` |
| The snapshot is sent after the prompt-cache breakpoint; the cached prefix is the same on every request (review) | `llm.TestTurnContextFollowsTheCachedPrefix`, `agent.TestHealthSnapshotAppearsInEveryTurn` |
| The snapshot is named as cluster data in the Trust rules; an empty one is stated (review) | `agent.TestSystemPromptHealthSnapshotRules`, `agent.TestHealthSnapshotFailureIsStated` |
| A pane that grows never leaves the view past the end; re-wrap and trimming keep the same text in view (review) | `tui.TestCopilotScrollIsClampedWhenThePaneGrows`, `tui.TestCopilotScrollKeepsItsPlaceAcrossRewrap`, `tui.TestCopilotScrollSurvivesTrimming` |

**The tests were checked against themselves** (task 4.2). Each behavior was
reverted in turn: the snapshot taken but left out of the system context
(`TestHealthSnapshotAppearsInEveryTurn` and `TestHealthSnapshotFailureIsStated`
failed); `renderChat` always going to the bottom
(`TestCopilotScrollIsNotYankedByNewOutput` and
`TestCopilotScrollSurvivesResizeAndMaximize` failed); the wheel routed
regardless of the pointer (`TestCopilotWheelScrollsOnlyOverThePane` failed).
The independent review's fixes (decisions K-48) were reverted the same way,
one at a time — `AnyFallback` instead of the screen's namespace, no adoption
on the snapshot's end, the snapshot inside the cached system prompt, an
empty snapshot passed through, no scroll anchor, the anchor's offset not
clamped — and each failed its test. One survived alone: the clamp on the
path with no anchor, which only runs before the first render and is
covered by the anchored path's clamp.

**Against a real API server** (this box's minikube, Kubernetes v1.35.1,
metrics-server running), on 2026-11-13: `healthSnapshot` from
`cmd/k8s-copilot`, run through a throwaway test (since deleted) against the
current context. The first call read the cluster in 51 ms and returned
5,111 bytes, under the 6,000 budget: the age line, all five classes
*watched*, and 17 Warning-event findings (probe failures after a node
reboot). The second call was served from the shared observation in under a
millisecond, with identical text. `list` requests only.

**Not done here** (task 4.3): the by-hand checks need a model API key, which
this environment does not have, and a person at a real terminal to turn the
wheel and drag. They are item 11 below. Notes on terminals for whoever runs
it: most emulators (xterm, GNOME Terminal, Konsole, kitty, Alacritty,
WezTerm, Windows Terminal) select with `shift`+drag while an application
reports the mouse; macOS Terminal uses `fn`+drag; iTerm2 uses `option`+drag
unless "Allow mouse reporting" is turned off; inside tmux, mouse events
reach k8s-copilot only with `set -g mouse on`, and selection then is
tmux's own (`shift`+drag passes it to the terminal). `ctrl+home`/`ctrl+end`
are not delivered by some terminals and multiplexers (e.g. older tmux
without `xterm-keys`); `pgup` and `pgdown` cover the same ground.

## Independent review

Before hand-over the approval path was reviewed adversarially by a separate
reviewer (see decisions K-24). It reproduced several problems with throwaway
tests; each one that was fixed now has a regression test:
`agent.TestApprovedButUnrecordableIsNotApplied`,
`audit.TestReplacedFileIsDetected`, `audit.TestTruncationWithHeadRemoved`,
`tui.TestApproveFlow` (typing), `tui.TestHideKeepsWaiting` (copilot pane),
`tui.TestDialogScrollsWithoutHidingContent`,
`tui.TestModelAndEventTextIsSanitized`, `tui.TestTypePickerSurvivesRefresh`,
`llm.TestRedirectsAreNotFollowed`.

## Against a real API server

`make live` (`K8S_COPILOT_LIVE=1 go test ./internal/tools -run Live -v`) uses your
current context. It only reads and sends `dryRun=All` requests, and asserts
afterwards that its target is unchanged. It was run against a local minikube
(Kubernetes v1.35.1) on 2026-10-06:

- connected, discovered 60 listable types;
- listed pods with the server's own columns (`NAME READY STATUS RESTARTS AGE`)
  and nodes without a namespace scope;
- `describe_resource` on `kube-system/coredns`;
- the API server **accepted, in dry-run, all four patch shapes** exactly as
  k8s-copilot sends them — including the merge patch on the `scale` subresource —
  and the Deployment was verified unchanged afterwards.

`K8S_COPILOT_LIVE_DEPLOYMENT=namespace/name` points it at another Deployment.

## Still to do by hand

These could not be done while building: they need a person at a real terminal,
a model API key being spent, and a change being approved on a cluster.

1. **Interactive smoke** (tasks 1.1, 2.6): start `bin/k8s-copilot`, move around
   (`n`, `t`, `enter`, `l`, `e`, `/`, `c`, `tab`), resize the window very
   small and back, quit with `q` and with `ctrl+c` from a few views, and
   confirm the terminal is left clean each time.
2. **Browse a CRD** (8.3): `t`, pick a custom resource, check the columns.
3. **Ask the copilot about a pod** (8.3): select a pod, `tab`, ask why it is
   restarting / whether it is healthy. Check the tool calls it lists make
   sense and the focus was understood without you naming the pod.
4. **Approve one `scale`** (8.3) on something harmless: ask it to scale a
   test Deployment, read the dialog, press `x` to see the request, `ctrl+y`
   `enter`. Then `kubectl get deploy` to confirm the effect and
   `k8s-copilot audit show` / `k8s-copilot audit verify` to confirm the record.
5. **Reject one and let one wait**: confirm nothing changes, and that `esc`
   leaves it waiting (`APPROVAL WAITING` in the header) for as long as you like.
6. **Out-of-scope requests** (8.4): ask it to delete a pod, to change an
   image, to apply a manifest. Expected: it explains it cannot, and tells you
   how to do it yourself; nothing is proposed.
7. **The metrics screen against a real metrics-server** (metrics change, task
   6.5). Everything about the happy path is tested with fakes and with an HTTP
   server that speaks metrics-server's wire format, but it has not been
   watched on a live cluster: the one available has no metrics-server, and
   installing one is a change to your cluster that was not this work's to
   make. On minikube: `minikube addons enable metrics-server`, wait a minute,
   press `M`. Compare the Nodes tab with `kubectl top nodes` and a namespace's
   pods with `kubectl top pods -n <ns> --containers`; drill into a pod; resize
   the window; then `minikube addons disable metrics-server` and watch the
   screen mark its readings as not current and, after a minute, say why there
   are none.
8. **Ask the copilot about load** (metrics change, task 6.6): with the metrics
   screen open, ask why a workload is slow. Expected: it calls `get_metrics`,
   and the figures it quotes are the ones on the screen.

9. **The health screen against minikube** (health change, task 5.3). In a
   throwaway namespace, force a pod into CrashLoopBackOff
   (`kubectl run crash --image=busybox -- sh -c 'exit 1'`) and a Deployment
   into a stalled rollout (`kubectl create deployment stall --image=nginx`,
   `kubectl patch deploy stall -p '{"spec":{"progressDeadlineSeconds":30}}'`,
   `kubectl set image deploy/stall nginx=nginx:does-not-exist`, wait a
   minute). Press `H`: expect a workloads and a rollouts finding (plus the
   pull failure and Warning events). Open logs and detail from each, filter,
   leave and return; the browser's namespace, type, selection and filter
   should be as they were. Then with a credential bound to one namespace only
   (a ServiceAccount with a namespaced Role, via its own kubeconfig context),
   confirm the affected classes say "namespace … only, because Permission
   denied … across all namespaces".
10. **Health usage against a metrics-server** (health change, task 5.4):
   `minikube addons enable metrics-server`; drive a container to its memory
   limit (`kubectl run hog --image=polinux/stress --limits=memory=64Mi --
   stress --vm 1 --vm-bytes 60M`); confirm one usage finding whose figure is
   the red row on the metrics screen. Then disable the addon and confirm the
   usage class says *not applicable* (or *unavailable* while it winds down) —
   never "no findings".

11. **Copilot health answer and scrolling** (findings-context change, task
   4.3). On a cluster with a known finding (item 9's crash loop will do),
   open the copilot and ask "what is broken?". Expected: the answer names
   that finding, and the transcript's tool lines show `get_findings` at most
   — no `list_resources`, `get_events` or `describe_resource` discovery pass.
   Ask something long enough to overflow the pane; turn the wheel over it
   with the browser focused, then over the browser (nothing should move);
   press `tab` and try `↑ ↓ pgup pgdn ctrl+u ctrl+d ctrl+home ctrl+end`, and
   `home`/`end` in a half-typed question; watch the title's range follow.
   Scroll up while an answer is still streaming in and confirm the view
   stays put, then send and confirm it jumps to the end. Finally `shift`+drag
   across the transcript and paste it somewhere. Record the terminal used.

A throwaway target for 4–6:

```sh
kubectl create namespace k8s-copilot-smoke
kubectl -n k8s-copilot-smoke create deployment web --image=nginx --replicas=1
# … try things …
kubectl delete namespace k8s-copilot-smoke
```
