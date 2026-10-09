# Configuration

Nothing is required. With no file, no variables and no flags, k8s-copilot connects
to your current kubeconfig context and uses Anthropic with `ANTHROPIC_API_KEY`.

Precedence, lowest to highest: **defaults → config file → `K8S_COPILOT_*`
environment → flags**. `k8s-copilot config` prints the result.

## Config file

`~/.config/k8s-copilot/config.yaml` (i.e. `$XDG_CONFIG_HOME/k8s-copilot/config.yaml`),
or the path in `--config` / `K8S_COPILOT_CONFIG`. Optional. Unknown keys are an
error, so typos do not pass silently.

```yaml
# Cluster — leave out to behave like kubectl
kubeconfig: ""            # default: $KUBECONFIG, then ~/.kube/config
context: ""               # default: the kubeconfig's current-context
namespace: ""             # namespace to start in; default: the context's

# Model
provider: anthropic       # anthropic | openai | openrouter | openai-compatible
model: ""                 # default: the provider's (anthropic: claude-opus-5-5)
base_url: ""              # required for openai-compatible
api_key_env: ""           # NAME of the variable holding the key (not the key)
max_tokens: 4096
request_timeout: 120s

# Bounds on one request to the copilot
max_iterations: 12        # model turns
max_tool_calls: 30        # tool calls (reads and proposals)

# Safety
audit_file: ""            # default: ~/.local/state/k8s-copilot/audit.jsonl
max_replicas: 100         # the most `scale` will propose
protected_namespaces: []  # namespaces in which nothing is ever proposed
redact_secrets: true      # hide Secret values from the model
max_result_bytes: 24000   # cap on one tool result sent to the model

# Browser
refresh_interval: 5s      # 0 or off disables periodic refresh
```

The metrics screen (`M`) has no settings of its own. It follows
`refresh_interval`, but never re-reads more often than every 10 s — the
cluster's metrics source has a new sample only every 15 s or so — and with
`refresh_interval: off` it reads when opened and when you press `r`.

API keys cannot be put in this file: a file containing `api_key` is rejected
with a message saying so.

## Environment

| Variable | Setting |
|---|---|
| `K8S_COPILOT_CONFIG` | config file path |
| `K8S_COPILOT_KUBECONFIG`, `K8S_COPILOT_CONTEXT`, `K8S_COPILOT_NAMESPACE` | cluster |
| `K8S_COPILOT_PROVIDER`, `K8S_COPILOT_MODEL`, `K8S_COPILOT_BASE_URL`, `K8S_COPILOT_API_KEY_ENV` | model |
| `K8S_COPILOT_MAX_ITERATIONS`, `K8S_COPILOT_MAX_TOOL_CALLS` | bounds |
| `K8S_COPILOT_AUDIT_FILE` | audit trail path |
| `K8S_COPILOT_REFRESH_INTERVAL` | e.g. `10s`, `off` |

API key variables (the default per provider; `api_key_env` overrides the name):

| Provider | Variable |
|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` |
| `openai` | `OPENAI_API_KEY` |
| `openrouter` | `OPENROUTER_API_KEY` |
| `openai-compatible` | `K8S_COPILOT_API_KEY` (optional — local servers often need none) |

`KUBECONFIG` is honoured exactly as kubectl honours it.

## Flags

```
--config FILE          config file
--kubeconfig FILE      kubeconfig file
--context NAME         kubeconfig context
--namespace, -n NAME   namespace to start in
--provider NAME        anthropic | openai | openrouter | openai-compatible
--model NAME           model
--base-url URL         provider API base URL
--api-key-env NAME     environment variable holding the API key
--refresh DURATION     listing refresh interval (0/off disables)
--max-iterations N     model turns per request
--max-tool-calls N     tool calls per request
--audit-file FILE      audit trail file
--version
```

Subcommands: `k8s-copilot audit verify`, `k8s-copilot audit show`, `k8s-copilot config`,
`k8s-copilot version`. The audit subcommands accept `--audit-file` (and `--config`).

## Examples

```sh
# Another context, starting in a namespace
k8s-copilot --context staging -n payments

# OpenRouter
export OPENROUTER_API_KEY=…
k8s-copilot --provider openrouter --model <vendor/model>

# A local model: nothing leaves the machine
k8s-copilot --provider openai-compatible --base-url http://localhost:11434/v1 --model llama3.1

# A work key in a differently named variable
k8s-copilot --provider openai --model <model> --api-key-env WORK_OPENAI_KEY

# Never propose anything in these namespaces
cat >> ~/.config/k8s-copilot/config.yaml <<'EOF'
protected_namespaces: [kube-system, prod]
EOF
```

Provider base URLs must be `https://`, except loopback addresses
(`localhost`, `127.0.0.1`, `::1`), because requests carry your API key and
cluster data.
