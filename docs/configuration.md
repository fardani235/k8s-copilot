# Configuration

Nothing is required. With no file, no variables and no flags, k2stui connects
to your current kubeconfig context and uses Anthropic with `ANTHROPIC_API_KEY`.

Precedence, lowest to highest: **defaults → config file → `K2STUI_*`
environment → flags**. `k2stui config` prints the result.

## Config file

`~/.config/k2stui/config.yaml` (i.e. `$XDG_CONFIG_HOME/k2stui/config.yaml`),
or the path in `--config` / `K2STUI_CONFIG`. Optional. Unknown keys are an
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
audit_file: ""            # default: ~/.local/state/k2stui/audit.jsonl
max_replicas: 100         # the most `scale` will propose
protected_namespaces: []  # namespaces in which nothing is ever proposed
redact_secrets: true      # hide Secret values from the model
max_result_bytes: 24000   # cap on one tool result sent to the model

# Browser
refresh_interval: 5s      # 0 or off disables periodic refresh
```

API keys cannot be put in this file: a file containing `api_key` is rejected
with a message saying so.

## Environment

| Variable | Setting |
|---|---|
| `K2STUI_CONFIG` | config file path |
| `K2STUI_KUBECONFIG`, `K2STUI_CONTEXT`, `K2STUI_NAMESPACE` | cluster |
| `K2STUI_PROVIDER`, `K2STUI_MODEL`, `K2STUI_BASE_URL`, `K2STUI_API_KEY_ENV` | model |
| `K2STUI_MAX_ITERATIONS`, `K2STUI_MAX_TOOL_CALLS` | bounds |
| `K2STUI_AUDIT_FILE` | audit trail path |
| `K2STUI_REFRESH_INTERVAL` | e.g. `10s`, `off` |

API key variables (the default per provider; `api_key_env` overrides the name):

| Provider | Variable |
|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` |
| `openai` | `OPENAI_API_KEY` |
| `openrouter` | `OPENROUTER_API_KEY` |
| `openai-compatible` | `K2STUI_API_KEY` (optional — local servers often need none) |

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

Subcommands: `k2stui audit verify`, `k2stui audit show`, `k2stui config`,
`k2stui version`. The audit subcommands accept `--audit-file` (and `--config`).

## Examples

```sh
# Another context, starting in a namespace
k2stui --context staging -n payments

# OpenRouter
export OPENROUTER_API_KEY=…
k2stui --provider openrouter --model <vendor/model>

# A local model: nothing leaves the machine
k2stui --provider openai-compatible --base-url http://localhost:11434/v1 --model llama3.1

# A work key in a differently named variable
k2stui --provider openai --model <model> --api-key-env WORK_OPENAI_KEY

# Never propose anything in these namespaces
cat >> ~/.config/k2stui/config.yaml <<'EOF'
protected_namespaces: [kube-system, prod]
EOF
```

Provider base URLs must be `https://`, except loopback addresses
(`localhost`, `127.0.0.1`, `::1`), because requests carry your API key and
cluster data.
