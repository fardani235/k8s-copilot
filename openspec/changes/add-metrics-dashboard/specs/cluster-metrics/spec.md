## ADDED Requirements

### Requirement: Read current usage from the cluster's metrics API, read-only

The system SHALL obtain current CPU and memory usage from the resource metrics API the cluster already serves, using only read requests made with the caller's existing kubeconfig credentials. It SHALL NOT request additional permissions, create or modify any cluster object, or require anything to be installed or configured.

#### Scenario: Usage is read

- **WHEN** the cluster serves the metrics API and the caller may read it
- **THEN** the system obtains current usage for nodes and for the containers of pods using list requests only

#### Scenario: Nothing is changed or installed

- **WHEN** usage is read, successfully or not
- **THEN** no create, update, patch or delete request is sent, and no cluster-side component is installed

### Requirement: Present load as a node, namespace, pod and container hierarchy

The system SHALL provide a metrics screen, reachable from any browser view with one key, that shows current CPU and memory usage per node, per namespace and per pod, and SHALL let the user drill down from the cluster to a single container and back up without losing their place.

#### Scenario: Open the metrics screen

- **WHEN** the user presses the metrics key in any browser view
- **THEN** the system shows the cluster's nodes with their usage, the cluster's totals, and how to reach the per-namespace and per-pod listings

#### Scenario: Drill down to a container

- **WHEN** the user selects a node or a namespace and drills in, then selects a pod and drills in
- **THEN** the system lists that node's or namespace's pods, and then that pod's containers, each with its usage

#### Scenario: Go back up

- **WHEN** the user goes back from a drilled-in listing
- **THEN** the system returns to the listing above it with the previously selected row selected, and from the top level returns to the view the screen was opened from

#### Scenario: Sort and filter

- **WHEN** the user changes the sort order or filters by name
- **THEN** the listing is reordered by CPU, memory or name, or reduced to the matching rows, and the selected row stays selected when it is still listed

#### Scenario: Open logs, events or detail from a row

- **WHEN** the user asks for the logs, events or detail of the selected row
- **THEN** the system opens the corresponding browser view for that pod, node or namespace and returns to the metrics screen at the same place when it is dismissed

### Requirement: Show usage against what bounds it

The system SHALL show usage together with what bounds it — a node's allocatable resources, and a pod's and container's requests and limits — and SHALL distinguish a bound that is not set from a bound that could not be read.

#### Scenario: Node usage against allocatable

- **WHEN** a node's usage and allocatable resources are both known
- **THEN** the system shows the usage and its share of allocatable

#### Scenario: Container usage against requests and limits

- **WHEN** a pod's containers are listed
- **THEN** each shows its usage, its request and limit, and its usage as a share of its limit

#### Scenario: No limit set

- **WHEN** a container declares no limit
- **THEN** the system says that none is set, and does not show a percentage, a zero, or the unknown marker in its place

#### Scenario: Near a hard bound

- **WHEN** usage reaches a high share of a node's allocatable resources or of a limit
- **THEN** the row is highlighted, and usage above a request alone is not treated as high

#### Scenario: One container at its limit

- **WHEN** one container of a pod is near its limit while the pod as a whole is not
- **THEN** the pod's row is highlighted and names that container

#### Scenario: Init container still running

- **WHEN** the metrics API reports usage for an init container that is still running
- **THEN** it is listed with the requests and limits its own specification declares, and the pod is measured against those rather than against containers that have not started

#### Scenario: Container not described by the pod specification

- **WHEN** the metrics API reports a container that the pod specification does not describe
- **THEN** its requests and limits, and the pod's, are shown as unknown rather than as not set

### Requirement: Unknown is never shown as zero

The system SHALL NOT present a reading it does not have as zero, as an empty value, or as an empty table. A missing reading SHALL be shown with an explicit unknown marker, and a measured zero SHALL be shown as zero.

#### Scenario: A row without a sample

- **WHEN** the metrics API returns no sample for a node, pod or container that is listed
- **THEN** its usage is shown with the unknown marker, the screen says why a sample may be missing, and the row is left out of percentages and totals rather than counted as zero

#### Scenario: A measured zero

- **WHEN** the metrics API reports zero usage for a container
- **THEN** the system shows zero

#### Scenario: Totals over partly reporting sets

- **WHEN** a total covers rows of which some have no sample
- **THEN** the total is computed over the reporting rows only and is labelled as covering only those

#### Scenario: A sample that measured nothing

- **WHEN** the metrics API returns a sample for a pod that lists no containers
- **THEN** the pod is shown as having no reading, not as using zero

#### Scenario: Small readings are added before they are rounded

- **WHEN** many containers each use a fraction of the smallest displayed unit
- **THEN** their total is the sum of what was measured, not the sum of the rounded figures

#### Scenario: A namespace that was not read

- **WHEN** a listing is asked for a namespace that the current readings do not cover
- **THEN** the system says the namespace was not read, and does not say that it has no pods

### Requirement: Say plainly when the metrics source is unavailable

When usage cannot be obtained, the system SHALL show an explicit state in place of the listing that says what is wrong, in terms that distinguish at least: the metrics API not being served by the cluster, the metrics API being served but not answering, the caller not being permitted to read it, and the request not completing in time. It SHALL include the server's own message when there is one.

#### Scenario: Metrics API not served

- **WHEN** the cluster does not serve the metrics API
- **THEN** the screen states that metrics are not available on this cluster and why, and shows no table and no zero readings

#### Scenario: Metrics API not answering

- **WHEN** the metrics API is registered but its backend does not answer
- **THEN** the screen states that the metrics API is not answering, distinctly from it not being installed

#### Scenario: Not permitted

- **WHEN** the cluster refuses the caller's request to read metrics
- **THEN** the screen states that this is a permission failure, not an idle cluster, and includes the server's reason

#### Scenario: Credentials not accepted

- **WHEN** the cluster does not accept the caller's credentials
- **THEN** the screen states that this is an authentication failure, distinctly from a missing permission

#### Scenario: One source missing, another available

- **WHEN** node metrics cannot be read but pod metrics can, or the reverse
- **THEN** the listing that depends on the missing source shows the unavailable state and the others work, and the screen says which still work

#### Scenario: Capacity or pod specifications unreadable

- **WHEN** usage can be read but node capacity or pod specifications cannot
- **THEN** usage is shown, the percentages or bounds that cannot be computed are shown as unknown, and the screen says why

#### Scenario: Pods readable only in one namespace

- **WHEN** the caller may not read pod metrics across the cluster but may in the browser's current namespace
- **THEN** the screen shows that namespace's pods and states that it is showing only that namespace and why

### Requirement: Live, with its age stated

The metrics screen SHALL refresh itself while it is visible, SHALL state how old its readings are, and SHALL NOT present readings that are no longer current as current.

#### Scenario: Periodic refresh

- **WHEN** the metrics screen is visible and the refresh interval elapses
- **THEN** the readings are read again and the screen updates, keeping the user's place

#### Scenario: Not visible

- **WHEN** the metrics screen is not visible
- **THEN** the system does not poll the metrics API

#### Scenario: Age is shown

- **WHEN** readings are shown
- **THEN** the screen shows the age of the sample, taking the older of the node and pod samples, and flags it when it is old

#### Scenario: Age keeps counting

- **WHEN** the metrics screen is left showing without any key being pressed, including with periodic refresh switched off
- **THEN** the age shown continues to increase

#### Scenario: Refresh fails after a successful read

- **WHEN** a refresh obtains no usage after an earlier one did
- **THEN** the earlier readings remain shown for a limited time, marked as not current together with the cause, and after that time the unavailable state is shown instead

### Requirement: Degrade without breaking

The metrics screen SHALL remain readable and SHALL NOT break the layout or block the interface when the terminal is small, when the metrics source is slow, or while a read is outstanding.

#### Scenario: Small terminal

- **WHEN** the terminal or the browser pane is narrow or short
- **THEN** the screen drops less important columns and detail, keeps names, usage and the age of the readings, and never draws outside its pane

#### Scenario: Terminal below the minimum

- **WHEN** the terminal is resized below the minimum usable dimensions while the metrics screen is open
- **THEN** the system shows its usual readable message instead of a broken layout, and restores the screen when the terminal is enlarged

#### Scenario: Slow metrics source

- **WHEN** the metrics API does not answer promptly
- **THEN** the interface stays responsive, the screen says that it is reading rather than showing an empty or zero state, no further read is started while one is outstanding, and the read is abandoned after a bounded time with a stated reason

### Requirement: One source of truth for the screen and the copilot

The system SHALL derive what the metrics screen shows and what the copilot is told from the same readings and the same computation, so that the two cannot differ in values, percentages, what is highlighted, or the explanation given when readings are unavailable.

#### Scenario: Same readings

- **WHEN** the copilot reads metrics while the metrics screen is showing recent readings
- **THEN** the copilot is given those readings rather than a separate, later read

#### Scenario: Same readings whatever the scope

- **WHEN** usage is asked for the whole cluster and for one namespace, in either order
- **THEN** both answers are taken from the same read, and readings held after a failed refresh are held for both

#### Scenario: The copilot had to read for itself

- **WHEN** the copilot reads metrics while the readings on the metrics screen are too old to reuse
- **THEN** the metrics screen takes up the readings the copilot was given, without a further read

#### Scenario: Same rows

- **WHEN** the copilot is given a listing that the screen can also show
- **THEN** every value in the copilot's listing equals the value on the screen for the same row and column

#### Scenario: Same explanation

- **WHEN** readings are unavailable
- **THEN** the copilot is told so in the same terms the screen uses
