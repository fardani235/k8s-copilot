# resource-browser Specification

## Purpose
Give users a fast, keyboard-driven way to explore any resource type in the active Kubernetes context from the terminal, and establish the live focus context that the copilot consumes.

## Requirements

### Requirement: Connect to the active kubeconfig context

The system SHALL establish a connection from the active kubeconfig context, resolved by the standard rules (the `KUBECONFIG` environment variable when set, otherwise `~/.kube/config`), and SHALL allow the context to be overridden by an explicit option.

#### Scenario: Connect using the active context

- **WHEN** the user starts the application and a valid kubeconfig with an active context exists
- **THEN** the system connects to that cluster and displays its resources

#### Scenario: Explicit context override

- **WHEN** the user starts the application with an explicit context option naming a context present in the kubeconfig
- **THEN** the system connects to that context's cluster instead of the active context

#### Scenario: No kubeconfig or no active context

- **WHEN** no kubeconfig is found or it defines no usable context
- **THEN** the system reports a clear, actionable error and exits without starting the browser

#### Scenario: Cluster unreachable or unauthorized

- **WHEN** the API server is unreachable or rejects the caller's credentials
- **THEN** the system reports the connection failure, including the server address and the authorization error when one is returned, and does not present an empty cluster as if it were healthy

### Requirement: Discover and browse any served resource type

The system SHALL discover the resource types served by the cluster's API and SHALL allow the user to select and list any discovered type, including cluster-scoped and namespaced types, without a hard-coded kind list.

#### Scenario: Browse a discovered resource type

- **WHEN** the user selects any resource type served by the cluster, such as pods, deployments, services, or a custom resource
- **THEN** the system lists the matching resources with kubectl-style columns including at minimum name, namespace where applicable, age, and status or readiness when available

#### Scenario: Cluster-scoped resource type

- **WHEN** the user selects a cluster-scoped resource type such as nodes or namespaces
- **THEN** the system lists it without implying a namespace scope

#### Scenario: Discovery unavailable

- **WHEN** the cluster's discovery information cannot be retrieved
- **THEN** the system reports the discovery failure and does not present a partial or misleading type list

### Requirement: Scope listings by namespace

The system SHALL list the namespaces available to the caller and SHALL scope resource listings to a selected namespace, with an option to list across all namespaces.

#### Scenario: Scope resources to a namespace

- **WHEN** the user selects a namespace
- **THEN** subsequent resource listings contain only resources in that namespace

#### Scenario: List across all namespaces

- **WHEN** the user selects the all-namespaces option
- **THEN** resource listings include resources from every namespace the caller can read, and each row identifies its namespace

#### Scenario: Empty listing

- **WHEN** the selected type has no matching resources in the current scope, including when the caller is not permitted to list that type
- **THEN** the system shows an explicit empty or permission-denied state rather than a blank or frozen view

### Requirement: Navigate resources and view detail and YAML

The system SHALL let the user navigate listings with the keyboard and SHALL show a detail view for the selected resource containing a summary of key fields and the complete resource manifest as YAML.

#### Scenario: Open a resource detail view

- **WHEN** the user selects a resource from a listing
- **THEN** the system shows a summary of key fields and the resource's YAML

#### Scenario: Return to the listing

- **WHEN** the user dismisses the detail view
- **THEN** the system returns to the previous listing with the previous selection and namespace scope intact

#### Scenario: Refresh listing

- **WHEN** the user requests a refresh, or the configured refresh interval elapses
- **THEN** the listing reflects the current cluster state

### Requirement: Inspect logs and events

The system SHALL show the logs of a selected workload's containers and the events associated with a selected resource.

#### Scenario: View container logs

- **WHEN** the user opens logs for a selected pod
- **THEN** the system shows that pod's logs, allows selecting among multiple containers, and supports following live output

#### Scenario: View events

- **WHEN** the user opens events for a selected resource
- **THEN** the system shows the events involving that resource, most recent last

#### Scenario: No logs available

- **WHEN** the selected container has produced no logs or has not started
- **THEN** the system shows an explicit empty state and does not hang

### Requirement: Expose current focus to the copilot

The system SHALL maintain and expose the current browser focus — the active namespace, the selected resource type, and the selected resource, when one is selected — so the copilot can include it in each agent turn.

#### Scenario: Focus changes are available to the copilot

- **WHEN** the user changes the namespace, resource type, or selected resource in the browser
- **THEN** the focus reported to the copilot reflects the change no later than the next agent turn

#### Scenario: No resource selected

- **WHEN** the browser has a namespace and type selected but no individual resource
- **THEN** the focus reported to the copilot describes the scope without claiming a selected resource

### Requirement: Keyboard-driven layout with copilot pane

The system SHALL provide keyboard navigation across namespaces, resource types, listings, and detail views; SHALL present contextual key hints; and SHALL expose the copilot as a pane that can be shown, hidden, focused, and maximized from the keyboard without losing browser context.

#### Scenario: Navigate and go back

- **WHEN** the user moves through the interface and navigates back
- **THEN** the system follows the focus and returns through the previous views without losing context

#### Scenario: Toggle the copilot pane

- **WHEN** the user toggles the copilot pane
- **THEN** the pane is shown or hidden while the browser's namespace, type, and selection are preserved

#### Scenario: Maximize the copilot pane

- **WHEN** the user presses the maximize key while the browser has focus
- **THEN** the copilot is shown if it was hidden, occupies the entire body area instead of half of it, and takes focus, and the browser listing, detail, logs, and events panes are not rendered

#### Scenario: Restore the split from maximized

- **WHEN** the copilot pane is maximized and the user presses a key that returns to the browser
- **THEN** the pane returns to the split layout — copilot beside browser — and the browser regains focus, so no pane is focused while hidden

#### Scenario: Browser state survives maximize and restore

- **WHEN** the copilot pane is maximized and then restored
- **THEN** the browser's namespace, resource type, selection, filter, and open sub-view are unchanged

#### Scenario: Quit from any view

- **WHEN** the user presses the quit key
- **THEN** the application exits cleanly and restores the terminal

#### Scenario: Terminal too small

- **WHEN** the terminal is resized below the minimum usable dimensions
- **THEN** the system shows a readable message instead of a broken layout
