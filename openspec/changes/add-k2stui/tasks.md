## 1. Project skeleton and cluster connection

- [ ] 1.1 Initialize the Go module and package layout (`cmd/k2stui`, `internal/...`) with a Bubbletea entry point that starts and quits; verify `go build ./...` and `go run ./cmd/k2stui` exits cleanly and restores the terminal
- [ ] 1.2 Implement kubeconfig resolution (standard rules plus explicit context override) and client construction; verify with a unit test covering `KUBECONFIG` precedence, default path, and override
- [ ] 1.3 Implement connection and error reporting for missing kubeconfig, no usable context, unreachable server, and unauthorized caller; verify with fake/httptest clients asserting the reported messages and non-zero exit

## 2. Resource browser

- [ ] 2.1 Implement cluster discovery of served resource types (namespaced, cluster-scoped, CRDs) and expose a selectable type list; verify discovery parsing with a fake discovery client and assert cluster-scoped types are not namespace-filtered
- [ ] 2.2 Implement namespace listing and selection with an all-namespaces option; verify scope filtering via a fake dynamic client
- [ ] 2.3 Implement resource listing through the dynamic client with a shared kubectl-style table renderer (name, namespace, age, status/readiness; discovery-derived columns with a safe fallback); verify column output against fixtures and assert every namespace row shows its namespace in all-namespaces mode
- [ ] 2.4 Implement the explicit empty and permission-denied states for a listing; verify with fake clients returning an empty list and a Forbidden error
- [ ] 2.5 Implement resource detail with key-field summary and full YAML, and returning to the listing with selection and scope preserved; verify with a round-trip UI/state test
- [ ] 2.6 Implement keyboard navigation, contextual key hints, refresh (manual plus interval) and clean quit from every view; verify the terminal-too-small message and that quit restores the terminal

## 3. Logs and events

- [ ] 3.1 Implement pod/container log retrieval with container selection, follow mode, and bounded buffering; verify log content and container switching with a fake typed client and assert an explicit empty state when no logs exist
- [ ] 3.2 Implement events for a selected resource, most recent last; verify ordering and filtering with a fake client

## 4. Copilot pane, provider-neutral loop

- [ ] 4.1 Implement the copilot pane as a toggleable side panel wired into the Bubbletea model and toggled from the keyboard; verify browser namespace/type/selection survive toggling
- [ ] 4.2 Define the provider interface (`SendTurn`: conversation in, tool calls or final message out) and a scripted stub provider; verify the stub drives a deterministic turn in tests
- [ ] 4.3 Implement the agent loop off the UI goroutine, including conversation history, bounded iterations/tool calls, and reporting when the bound is reached; verify with the stub provider that a capped run stops and reports the limit
- [ ] 4.4 Implement browser focus injection (namespace, type, selected resource) into each turn's context; verify the stub observes the current focus after a focus change
- [ ] 4.5 Implement one concrete provider adapter plus credential/config loading from the environment; verify a missing credential produces a clear error and a provider failure leaves the browser usable and applies nothing

## 5. Typed tool registry

- [ ] 5.1 Implement the registry with named typed tools, argument schemas, and read/mutate classification; verify unknown or malformed tool calls return a descriptive error and execute nothing
- [ ] 5.2 Implement read tools (`list_resources`, `get_resource`, `describe_resource`, `get_logs`, `get_events`) that auto-execute; verify with fake clients that results are returned to the loop
- [ ] 5.3 Implement mutate tools (`scale`, `rollout_restart`, `set_labels`, `set_annotations`) as gated operations only, and reject delete/arbitrary-apply/image-change requests with an explanation; verify each supported verb's patch shape against a fake client and that unsupported verbs are refused
- [ ] 5.4 Implement server-side dry-run validation ahead of the gate; verify a rejected dry-run never reaches the approval UI and its reason is returned to the model, and a valid one is presented as validated
- [ ] 5.5 Assert that no code path shells out or builds command strings for cluster operations; verify by test and code review (no `os/exec` used for cluster access)

## 6. Approval flow

- [ ] 6.1 Implement the approval dialog content (verb, target, before→after, dry-run result, reversibility) and approve/reject/edit/explain actions; verify rendered content for a scale and a label patch fixture
- [ ] 6.2 Implement the blocking gate: the loop blocks on a channel until the human decides, with no timeout or default; verify with the stub that an unanswered proposal never applies and a rejection returns a "declined by user" tool result so the loop resumes
- [ ] 6.3 Implement apply-after-approval and its failure path (conflict, RBAC, admission); verify with a fake client that a post-approval failure is returned to the model as the tool result and is audited
- [ ] 6.4 Verify the end-to-end loop with the stub provider: read → dry-run → approval → apply → real result → continue, plus the reject and dry-run-reject branches

## 7. Audit trail

- [ ] 7.1 Implement the append-only JSONL audit file with the required fields (time, intent, tool, target, args, dry-run result, decision, outcome); verify one entry per approved, rejected, dry-run-rejected, and failed action
- [ ] 7.2 Implement the hash-chain integrity field and a verification routine; verify an edited or truncated entry is detected and the application exposes no modify/delete path
- [ ] 7.3 Implement a clear failure when the audit file cannot be written and ensure the action is not treated as successfully audited; verify entries persist across a restart

## 8. Integration, configuration, and hardening

- [ ] 8.1 Add configuration for provider selection, credential env var, refresh interval, iteration/tool-call limits, audit path, and context override; verify defaults and overrides load from config
- [ ] 8.2 Run the full test suite with `go test ./...` and `go vet ./...`; verify all pass
- [ ] 8.3 Perform a manual smoke test against the local cluster: browse a discovered CRD, ask the copilot about a selected pod, approve one `scale`, and confirm the audit entry and cluster effect
- [ ] 8.4 Confirm the documented out-of-scope mutations (delete, arbitrary apply/patch, image change) are refused in the running binary
