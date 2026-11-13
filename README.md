# k8s-copilot

A terminal Kubernetes browser with an AI copilot beside it.

Browse the cluster the way you already do, and ask it things — *"why does this
pod keep restarting?"* — and it goes and looks: logs, events, owners, config,
CPU and memory. It comes back with an explanation, and when the fix is clear it
can offer to make it.

**It can look at anything you can. It changes nothing on its own.** When it
wants to change something it shows you exactly what, and waits — for as long as
you like. No timeout, no default. Nothing happens until you say yes, and if you
say no it moves on.

```
 k8s-copilot  ctx minikube  ns shop  type pods                      APPROVAL WAITING — ctrl+p
╭──────────────────────────────────────────────────────╮╭──────────────────────────────────╮
│pods · namespace shop · 3                             ││copilot · anthropic/claude-opus-… │
│NAME    READY  STATUS            RESTARTS      AGE    ││you                               │
│web-1   0/1    CrashLoopBackOff  12 (2m ago)   41m    ││why does this pod keep restarting?│
│web-2   1/1    Running           0             41m    ││· describe_resource(name=web-1, …)│
│db-0    1/1    Running           0             3d     ││· get_logs(pod=web-1, previous=…) │
│                                                      ││copilot                           │
│                                                      ││It is OOM-killed about 40s after  │
│                                                      ││start: "exit 137, OOMKilled" …    │
╰──────────────────────────────────────────────────────╯╰──────────────────────────────────╯
 ↑↓ move · enter detail · l logs · e events · n namespace · t type · / filter · c copilot · tab ask
```

## Quick start

Requires Go 1.26+ to build. No cluster-side install, no new credentials.

```sh
make build                     # → bin/k8s-copilot
export ANTHROPIC_API_KEY=…     # or pick another provider, see below
bin/k8s-copilot                     # uses your current kubeconfig context
```

It connects exactly the way `kubectl` does: `$KUBECONFIG`, else
`~/.kube/config`, current context — including exec/OIDC credential helpers.
`--context` and `-n` override the context and starting namespace.

Without an API key the browser still works; the copilot pane tells you what to
set.

### Choosing the model

| `--provider`         | Key read from        | Notes                                              |
|----------------------|----------------------|----------------------------------------------------|
| `anthropic` (default)| `ANTHROPIC_API_KEY`  | default model `claude-opus-5-5`; `--model` changes it |
| `openai`             | `OPENAI_API_KEY`     | `--model` required                                 |
| `openrouter`         | `OPENROUTER_API_KEY` | `--model` required, e.g. any model OpenRouter lists |
| `openai-compatible`  | `K8S_COPILOT_API_KEY` (optional) | `--base-url` + `--model`; Ollama, vLLM, LiteLLM, a gateway… |

```sh
bin/k8s-copilot --provider openrouter --model <vendor/model>
bin/k8s-copilot --provider openai-compatible --base-url http://localhost:11434/v1 --model llama3.1
```

The same settings can live in `~/.config/k8s-copilot/config.yaml` or `K8S_COPILOT_*`
variables — see [docs/configuration.md](docs/configuration.md). `k8s-copilot config`
prints what is in effect (never the key).

## Using it

**Browser** (works on every resource type the cluster serves, CRDs included):

| Key | |
|---|---|
| `↑ ↓ j k`, `pgup pgdn`, `g G` | move |
| `enter` | detail: key fields + full YAML |
| `l` | logs of a pod (`f` follow, `s` switch container, `v` previous instance) |
| `e` | events for the selected resource, most recent last |
| `n` / `t` | choose namespace (or all) / resource type — type to filter |
| `/` | filter the listing |
| `r` | refresh now (it also refreshes every 5s) |
| `esc` | back |
| `M` | metrics: how loaded the cluster is right now ([below](#metrics)) |
| `H` | health: what is broken right now ([below](#health)) |
| `A` | audit trail |
| `?` | help |
| `q`, `ctrl+c` | quit |

**Copilot**: `c` shows/hides the pane, `m` maximizes it to fill the body
(`esc` or `tab` restores the split), `tab` moves between browser and copilot,
`enter` sends, `esc` cancels a running request, `ctrl+l` starts a new
conversation. It is told what you are looking at (context, namespace, type,
selected resource) with every question, so "this pod" just works.

### Metrics

`M`, from anywhere in the browser, shows how busy the cluster is right now —
nodes, namespaces and pods, down to a single container:

```
 metrics · cluster                                              sampled 8s ago · 15s window
 cluster  █████░░░░░ CPU 3.1 of 6 allocatable (52%)   ████████░░ memory 9.8Gi of 12Gi allocatable (82%)
 2 nodes · 4 pods in all namespaces
  1 nodes (2)  2 namespaces (2)  3 pods (4)   by cpu ↓
 NODE    CPU  %CPU                MEM  %MEM              PODS
 node-b  1.9   95%  ██████████  3.8Gi   95%  ██████████     1
 node-a  1.2   30%  ███░░░░░░░    6Gi   75%  ████████░░     2
```

| Key | |
|---|---|
| `1` `2` `3`, `← →` | nodes / namespaces / pods |
| `enter` | drill down: a node's or a namespace's pods, then a pod's containers |
| `esc` | back up one level, then out |
| `s` | sort by cpu, memory, name |
| `/` | filter by name |
| `l` `e` `d` | logs, events, detail of the selected row |
| `r` | read again now (it also re-reads by itself, about every 10s) |

Usage is shown against what bounds it: a node's allocatable, a pod's requests
(`%CPU/R`, `%MEM/R`) and limits (`%CPU/L`, `%MEM/L`). A row turns amber at 75%
and red at 90% of a hard bound — close to a memory limit an OOM kill is near,
at a CPU limit the container is being throttled.

It reads the cluster's own metrics API, the one `kubectl top` uses. It is
read-only, needs no extra permission, and installs nothing. So when that API is
not there, the screen says so instead of showing an empty table:

```
 Metrics are not available on this cluster

 The API server does not serve the metrics API (metrics.k8s.io), so there are
 no CPU or memory readings to show. That usually means metrics-server, or an
 equivalent, is not installed. k8s-copilot only reads what the cluster already
 offers: it installs nothing.
```

"Not installed", "installed but not answering", "you are not allowed to read
it", "your credentials were not accepted" and "too slow" are five different
messages. A reading that is missing is
drawn as `—`, **never as 0**; a limit that is not set is `no lim`. If you may
read metrics only in your own namespace, you get that namespace and are told
that is what you are looking at. If a refresh fails, the last readings stay for
up to a minute, marked as not current.

The copilot reads the same numbers (`get_metrics`) — the very readings on the
screen if it is open — so when you ask why something is slow it can check
whether it is actually CPU-saturated or short of memory, and what it tells you
matches what you see. There is no history: this is a picture of now.

### Health

`H`, from anywhere in the browser, answers "is anything broken right now?" —
and says just as plainly what it did not look at:

```
 health · 2 findings                                              read 4s ago
 workloads watched · 1
 nodes     watched
 rollouts  watched · 1
 usage     not applicable — Metrics are not available on this cluster: …
 events    watched
 WRK Pod shop/web-1 [app]  CrashLoopBackOff  3m
 ROL Deployment shop/api  ProgressDeadlineExceeded  8m

 for 3m · container app is crash-looping: restarted 7 times; last exit: Error, exit code 1
```

Five classes, each with the line that decides it:

- **workloads** — a container in CrashLoopBackOff or restarted more than 5
  times; an image that cannot be pulled; a container stuck waiting
  (CreateContainerConfigError, RunContainerError, …) or a running pod none of
  whose containers has been ready, for over 5 minutes;
- **nodes** — NotReady or no Ready condition, memory pressure, disk pressure;
- **rollouts** — a Deployment that reports it is not progressing; a
  StatefulSet or DaemonSet short of ready replicas with a pod not ready for
  over 5 minutes (paused and completed rollouts are never reported);
- **usage** — a node or container at the metrics screen's red, 90% of its
  allocatable or its limit;
- **events** — Warning events of the last 15 minutes, repeats grouped with a
  count.

| Key | |
|---|---|
| `↑ ↓ j k` | move |
| `enter`, `d` | detail of the finding's object |
| `l` `e` | logs (pods), events of the finding's object — `esc` comes back to the same finding |
| `/` | filter by object, namespace, class or reason |
| `r` | read again now (it also re-reads by itself, at most every 15s, only while showing) |
| `esc` | clear the filter, then back to where you were |

**A class it could not read is not a class with no problems.** Each class
says whether it was *watched*, *watched in part* (only your namespace, because
the cluster-wide read was refused; or only the first 500 objects), *forbidden*,
*unavailable* or *not applicable* (no metrics API) — and why. Only when every
class was watched across the whole cluster does it say "No findings". It is
read-only, needs no extra permission and installs nothing.

**What it does not cover**: failed Jobs or CronJobs, Pending pods that cannot
be scheduled or are stuck in ContainerCreating, StatefulSets or DaemonSets
whose pods were never created, PersistentVolumeClaims,
HorizontalPodAutoscalers, PodDisruptionBudgets, Service endpoints. A crash-looping pod also shows up
under events; the two are not merged. There is no history and no
notification: it is a picture of now, with its age on show.

The copilot reads the same report (`get_findings`) — the one on the screen if
it is open — so "what is broken?" gets the findings you see, and it is told
never to call a class healthy that was not watched.

### Changes

**When it proposes a change** a dialog takes over the screen:

```
 APPROVAL NEEDED   nothing has been changed

 Scale Deployment shop/web from 2 to 4 replicas

 Action     scale
 Target     Deployment shop/web  (apps/v1)
 Cluster    minikube  https://192.168.49.2:8443
 Change     spec.replicas:  2  →  4
 Dry-run    ✓ passed — the API server accepted this exact request with dryRun=All
 Undo       Reversible: scale back to 2. …

 You asked  why is web slow?
 Its reason CPU is saturated on both replicas…  (the model's words, not verified)

 ctrl+y approve · ctrl+n reject · e edit · x exact request · esc hide (keeps waiting)
 There is no timeout. Nothing happens until you decide.
```

- `ctrl+y` then `enter` approves; `ctrl+n` rejects. They are chords on
  purpose: if the dialog pops up while you are still typing, nothing you type
  can answer it.
- `e` lets you edit the arguments; the edited proposal is validated again and
  shown again before anything happens.
- `x` shows the literal API request that would be sent.
- `esc` only hides the dialog — the proposal keeps waiting and `ctrl+p` brings
  it back. You can browse meanwhile.

Everything in that dialog except "Its reason" is computed by k8s-copilot from the
cluster, not written by the model.

## What it can and cannot do

| | |
|---|---|
| **Reads, freely** | list any resource type, get, describe, pod logs, events, current CPU and memory usage, current health findings |
| **Proposes, and waits** | `scale` (Deployment/StatefulSet/ReplicaSet), `rollout_restart` (Deployment/StatefulSet/DaemonSet), `set_labels`, `set_annotations` |
| **Cannot, at all** | delete, create, apply or patch manifests, change images, exec, port-forward, cordon/drain — there is no code path for these |

This is enforced in code, not in the prompt. See
[docs/security.md](docs/security.md) for exactly how, and what the limits are.

## The audit trail

Every proposal is recorded — approved, rejected, edited, rejected by the API
server's dry-run, failed on apply, or abandoned — with what you asked, what it
proposed (and why), what you decided, and what happened. An approval is written
to disk *before* the change is sent and the result right after, so there is no
moment in which a change exists without a record.

```sh
k8s-copilot audit show      # read it
k8s-copilot audit verify    # check nobody altered it (exit 1 if they did)
```

It lives at `~/.local/state/k8s-copilot/audit.jsonl` (JSON Lines, hash-chained,
append-only, mode 0600) and is also a keypress away inside the app (`A`). If
the trail cannot be written, k8s-copilot stops proposing changes: no record, no
change.

## Documentation

- [docs/architecture.md](docs/architecture.md) — how it is put together
- [docs/security.md](docs/security.md) — the safety model, threat model and known limits
- [docs/decisions.md](docs/decisions.md) — every design decision, including where the implementation departs from the original design and why
- [docs/configuration.md](docs/configuration.md) — all settings
- [docs/testing.md](docs/testing.md) — what is tested, how, and what still needs a human
- [CHANGELOG.md](CHANGELOG.md)
- [openspec/changes/archive/2026-10-07-add-k2stui/](openspec/changes/archive/2026-10-07-add-k2stui/) — the requirements this implements
- [openspec/changes/add-metrics-dashboard/](openspec/changes/add-metrics-dashboard/) — the metrics screen: proposal, design (with the alternatives considered) and requirements
- [openspec/changes/add-cluster-health/](openspec/changes/add-cluster-health/) — the health screen and `get_findings`

## Status

First implementation of the `add-k2stui` change. The automated suite passes and
the read paths and all four patch shapes have been checked (dry-run only)
against a real API server. Two things from the task list still need you at a
keyboard — an interactive smoke test with a real model, and one approved change
end to end. See [docs/testing.md](docs/testing.md#still-to-do-by-hand).

The metrics screen (`add-metrics-dashboard`) is implemented and tested, and its
"no metrics API" state has been checked against a real API server and in a real
terminal. It has **not** yet been watched against a real metrics-server — see
the same section.

The health screen (`add-cluster-health`) is implemented and tested, and its
reads and `get_findings` have been run read-only against a real API server
with metrics-server. The interactive checks — forcing a crash loop and a
stalled rollout, a namespace-only credential, a container at its limit — have
**not** yet been done by hand; see the same section.
