# Changelog

## Unreleased — metrics screen (`openspec/changes/add-metrics-dashboard`)

### Added

- **Metrics screen** (`M`, from any browser view): current CPU and memory per
  node, per namespace and per pod, with drill-down to one container (node or
  namespace → pods → containers) and a breadcrumb back up. Usage is shown
  against what bounds it — a node's allocatable, a pod's and a container's
  requests and limits — with rows near a hard bound highlighted. Sort (`s`),
  filter (`/`), and `l` / `e` / `d` to jump to the logs, events or detail of
  the selected row. It refreshes itself while visible and shows the age of its
  sample.
- **`get_metrics` copilot tool** (read tier): the same four levels — nodes,
  namespaces, pods, containers — returning the same readings as the screen.
  The system prompt tells the model to check load with it instead of inferring
  it, and what an unavailable source means.
- **`internal/metrics`**: one pure model (`Snapshot` → `Query` → `Listing`)
  that both the screen and the tool render, so they cannot differ in numbers,
  percentages, what is flagged, or wording.
- **`kube.Cluster.Metrics`**: four `list` reads (node metrics, pod metrics,
  nodes, pods) through the existing dynamic client, each with its own status,
  behind a small cache so the screen and the copilot share one snapshot.
- **Explicit states instead of zeros.** Metrics API not installed (404),
  installed but not answering (503), not permitted (403), credentials not
  accepted (401), timed out — each
  said in its own words, on the screen and to the model, with the server's
  message. A missing reading is `—`, never `0`; a missing source is never a
  table. Readings kept after a failed refresh are marked as not current and
  dropped after a minute.
- **Partial permissions.** Each source fails on its own: a user who cannot
  list nodes still gets pod metrics; a user limited to one namespace gets that
  namespace, with a note saying so.
- **Fake cluster support** for the metrics API in `internal/kube/kubetest`
  (not served by default, like a cluster without metrics-server).
- **Tests**: 64 new top-level tests (161 in all), including the wire format
  of a real metrics-server over real HTTP, and an opt-in live test that sends
  `list` requests only (`-run LiveMetrics`).

### Changed

- The registry is six read tools and four mutate tools (was five and four).
- The browser's focus, as sent to the copilot, describes the metrics screen
  when it is open (what is listed, which row is selected).
- The filter input (`/`) now serves whichever listing is on screen; the
  resource listing's filter and the metrics screen's are kept separately.
- `make race` also covers `internal/kube` and `internal/metrics`.

### Fixed

- The "copilot is not available" text was not cut to its pane: a long reason
  on a short terminal pushed the pane's bottom border off the screen. Found
  while checking the metrics screen at 60×16 with a real configuration.

### Unchanged, on purpose

- **Read-only, no new permissions, nothing installed.** `go.mod` is untouched;
  `internal/guard` passes without modification (still one write call in the
  program, one raw HTTP request and it is a GET, no subprocess).
- No new configuration. The screen follows `refresh_interval`, with a floor of
  10 s.

### Reviewed

An independent adversarial review found no way to make a missing source look
like zero usage, and no race, deadlock or non-`list` request. It did find
defects, chiefly ways the copilot and the screen could be given different
readings; each was reproduced, fixed and given a failing-without-the-fix test
before hand-over (`docs/decisions.md` K-38).

### Not yet verified

- The metrics screen against a **real metrics-server**. The "no metrics API"
  state was checked against a real API server and in a real terminal; the
  cluster available had no metrics-server and none was installed for the
  purpose. See `docs/testing.md` (still to do by hand, items 7 and 8).

### Decisions

`docs/decisions.md` K-25 – K-38; the alternatives considered are in
`openspec/changes/add-metrics-dashboard/design.md`.

## Unreleased — first implementation (`openspec/changes/archive/2026-10-07-add-k2stui`)

Greenfield: the repository previously held only the OpenSpec documents.

### Added

- **Go module** `github.com/fardani235/k8s-copilot`, single binary `cmd/k8s-copilot`
  (Bubble Tea / Lip Gloss / Bubbles, client-go).
- **Resource browser** (`internal/kube`, `internal/tui`): connects via the
  standard kubeconfig rules (`--context`, `--kubeconfig`, `-n` overrides);
  generic discovery of every listable type including CRDs; kubectl-style
  columns from the API server's Table printer with a generic fallback;
  namespace scoping and all-namespaces; detail view (key fields + YAML); pod
  logs (container switch, follow, previous instance, bounded buffer); events
  (most recent last); filter; manual and periodic refresh; explicit empty,
  permission-denied, discovery-failed and terminal-too-small states.
- **Copilot** (`internal/agent`, `internal/llm`): a locally owned, bounded
  agent loop behind a one-method provider interface. Providers: Anthropic,
  OpenAI, OpenRouter, any OpenAI-compatible endpoint; a scripted stub for
  tests. Browser focus is injected into every request.
- **Typed tool registry** (`internal/tools`): read tools `list_resources`,
  `get_resource`, `describe_resource`, `get_logs`, `get_events`; gated mutate
  tools `scale`, `rollout_restart`, `set_labels`, `set_annotations`. Strict
  argument decoding. Nothing else exists; out-of-scope requests are refused
  with an explanation.
- **Approval gate** (`internal/approval`, `internal/tui/modal.go`):
  server-side dry-run before the human is asked; a dialog with action, target,
  cluster, before→after, dry-run verdict, reversibility, warnings and the exact
  request; approve (`ctrl+y`, `enter`) / reject (`ctrl+n`) / edit
  (re-validated) / hide. Blocks
  indefinitely; no timeout or default.
- **Audit trail** (`internal/audit`): append-only hash-chained JSONL at
  `~/.local/state/k8s-copilot/audit.jsonl`; `k8s-copilot audit show`, `k8s-copilot audit
  verify`, and an in-app view (`A`). An approval is recorded before the
  change is sent and its result after. Fail-closed: no working trail, no
  proposals.
- **Configuration** (`internal/config`): defaults < file < `K8S_COPILOT_*` < flags;
  `k8s-copilot config` prints the effective settings.
- **Safety hardening not in the original spec**: approval grants bound to the
  exact request; stale-state check before apply; Secret redaction for the
  model; terminal-escape sanitisation of all cluster/model text; replica cap;
  system-managed label/annotation keys refused; optional protected namespaces;
  https-only provider URLs; source-level guard tests.
- **Tests**: 97 top-level tests (150 including sub-tests) across all packages;
  `-race` clean on the concurrent packages; an opt-in live test (read-only +
  dry-run).
- **Docs**: README, `docs/architecture.md`, `docs/security.md`,
  `docs/decisions.md`, `docs/configuration.md`, `docs/testing.md`.

### Departures from the design document

See `docs/decisions.md` — K-01 (plain HTTP adapters instead of vendor SDKs),
K-02 (scale via merge patch on the scale subresource), K-03 (stale-state check
instead of resourceVersion preconditions), K-04 (partial discovery shown with a
warning), K-11 (an approved action has two audit entries — write-ahead — rather
than one).

### Reviewed

An independent adversarial review of the approval path found no way to apply an
unapproved change; the issues it did find were fixed before hand-over
(`docs/decisions.md` K-24).

### Not yet verified

The interactive smoke test with a real model and one approved change on a
cluster (tasks 8.3, 8.4) — see `docs/testing.md`.
