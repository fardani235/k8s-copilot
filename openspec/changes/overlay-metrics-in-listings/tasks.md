# Tasks

## 1. One set of usage cells

- [ ] 1.1 Export `metrics.PodUsageCells` (CPU, %CPU/R, %CPU/L, MEM, %MEM/R, %MEM/L) and `metrics.NodeUsageCells` (CPU, %CPU, MEM, %MEM of allocatable), refactor `listPods` and `listNodes` to fill their usage cells from them, and verify the existing `internal/metrics` tests still pass unchanged.
- [ ] 1.2 Add unit tests for the constructors covering a missing reading (`—`), a measured zero (`0m` / `0`), a bound known to be absent (`no req` / `no lim`), and a bound unknown (`—`); verify with `go test ./internal/metrics/`.

## 2. Overlay in `internal/kube`

- [ ] 2.1 Add `UsageOverlay` to `kube.Table` and `AttachUsage`, appending the columns and cells for core `pods` and `nodes` only, recording `From`, drop ranks, the one-line note and the snapshot; verify with new `internal/kube` tests using `kubetest` that a pod and a node row carry the expected cells.
- [ ] 2.2 Implement the absence rules in `AttachUsage`: no columns when node or pod metrics is absent, unavailable, denied, unauthenticated, timed out or failed, and no columns when a namespace-scoped read meets an all-namespaces listing; verify each state with a test that asserts `len(Columns)` is unchanged and `Note` names the cause.
- [ ] 2.3 Implement the "no all-unknown column" and per-row-unknown rules: drop a column whose cells are all `NoData`, keep `no req` / `no lim` / `none` columns, and show `—` per row; verify with tests where the pod objects are unreadable and where one pod has no sample.
- [ ] 2.4 Implement `Cluster.ListWithUsage`: run the listing and the metrics read concurrently, give the metrics read `UsageOverlayWait` (3s), and attach the snapshot; verify with `kubetest` that a blocked metrics handler returns the listing promptly with no columns, that a later call finds the completed read, and that `MetricsCalls` does not grow within `MetricsFreshFor`.
- [ ] 2.5 Implement `UsageOverlay.Line(now)` for the fresh, stale and held cases using `metrics.Freshness`; verify with tests asserting the exact sentence and that `warn` is set for stale and held.

## 3. Browser listing

- [ ] 3.1 Render the overlay columns in `viewList` and print `Overlay.Line(time.Now())` once above the table, with warn styling when it says so; verify with TUI tests that columns appear, the line appears, and the row count still fills the pane.
- [ ] 3.2 Exclude overlay cells from the filter by joining `r.Cells[:Overlay.From]` in `refilter`; verify that filtering by a usage value does not match and that the previous filter behaviour is otherwise unchanged.
- [ ] 3.3 Drop overlay columns on a narrow pane in the order request/limit percentage, `MEM`, `CPU`, before any server column; verify with a test at a narrow width that names, status and age remain and the overlay columns reduce in that order.
- [ ] 3.4 Add a listing clock that redraws while a listing with an overlay is visible, so the stated age keeps counting and the stale/held flag appears with periodic refresh switched off; verify with a model test that advances the clock and sees the line change, and that no cluster read is issued.
- [ ] 3.5 Confirm selection, `enter`/`l`/`e`, refresh and namespace/type changes still work over the wider table; verify with the existing TUI tests plus a test that a refresh keeps the selected pod.

## 4. Copilot listing

- [ ] 4.1 Make `list_resources` use `ListWithUsage` and print `Overlay.Line(now)` before the table; verify with `internal/tools` tests that pods and nodes carry the columns, that an unavailable source yields no columns and the same sentence, and that a row without a sample shows `—`.
- [ ] 4.2 Verify the copilot's values equal the metrics screen's for the same pod and node by asserting against the same `metrics` listing cells; verify with a tool test built on a `kubetest` cluster.

## 5. Documentation

- [ ] 5.1 Update README (columns in the pods/nodes listings and when they are absent), `docs/architecture.md` (the overlay step and its two consumers), `docs/decisions.md` (new K-49…K-5x entries for the overlay location, the 3 s budget, the column-of-`—` rule and the filter exclusion) and `docs/testing.md`; verify the documented behaviour matches the code and the changed test names.
- [ ] 5.2 Add the CHANGELOG entry under the current Unreleased section, including that no new permission, dependency or configuration was added; verify `internal/guard` passes untouched.

## 6. Integration

- [ ] 6.1 Run `make check` and `make race` and verify both pass with the new tests included.
- [ ] 6.2 Run a `kubetest` end-to-end test that shows the same pod in the browser model and through `list_resources` and asserts every usage cell is equal, then repeats with the metrics API absent and asserts neither carries a usage column.

## Workflow follow-up

- Archive the change after the project's review requirements are satisfied.
- Verify the archived result.
