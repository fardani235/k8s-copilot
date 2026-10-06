# Testing

```sh
make test     # go test ./...        — no cluster and no model needed
make vet      # go vet + gofmt check
make race     # the concurrent packages under -race
make live     # opt-in: read-only + dry-run against your CURRENT kube context
```

## How it is tested without a cluster or a model

- **Cluster**: `internal/kube/kubetest` builds a `kube.Cluster` on client-go's
  fake dynamic and typed clients, with a fake discovery that can fail. The
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
| 1.1 skeleton | `go build ./...`; `cmd/k2stui` tests. Terminal restore: by hand (below) |
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

`make live` (`K2STUI_LIVE=1 go test ./internal/tools -run Live -v`) uses your
current context. It only reads and sends `dryRun=All` requests, and asserts
afterwards that its target is unchanged. It was run against a local minikube
(Kubernetes v1.35.1) on 2026-10-06:

- connected, discovered 60 listable types;
- listed pods with the server's own columns (`NAME READY STATUS RESTARTS AGE`)
  and nodes without a namespace scope;
- `describe_resource` on `kube-system/coredns`;
- the API server **accepted, in dry-run, all four patch shapes** exactly as
  k2stui sends them — including the merge patch on the `scale` subresource —
  and the Deployment was verified unchanged afterwards.

`K2STUI_LIVE_DEPLOYMENT=namespace/name` points it at another Deployment.

## Still to do by hand

These could not be done while building: they need a person at a real terminal,
a model API key being spent, and a change being approved on a cluster.

1. **Interactive smoke** (tasks 1.1, 2.6): start `bin/k2stui`, move around
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
   `k2stui audit show` / `k2stui audit verify` to confirm the record.
5. **Reject one and let one wait**: confirm nothing changes, and that `esc`
   leaves it waiting (`APPROVAL WAITING` in the header) for as long as you like.
6. **Out-of-scope requests** (8.4): ask it to delete a pod, to change an
   image, to apply a manifest. Expected: it explains it cannot, and tells you
   how to do it yourself; nothing is proposed.

A throwaway target for 4–6:

```sh
kubectl create namespace k2stui-smoke
kubectl -n k2stui-smoke create deployment web --image=nginx --replicas=1
# … try things …
kubectl delete namespace k2stui-smoke
```
