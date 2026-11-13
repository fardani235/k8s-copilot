# agent-copilot Specification

## Purpose
Let a language model investigate the cluster through typed tools and propose changes that a human must approve before they are applied.

## Requirements

### Requirement: Provider-neutral agent loop

The system SHALL run the agent loop itself and SHALL communicate with the model through a provider interface, so the model provider is selected by configuration and can be changed without altering the loop's behavior.

#### Scenario: Configured provider is used

- **WHEN** the application starts with a provider selected in configuration and the required credential present
- **THEN** agent turns are sent to that provider and the loop runs normally

#### Scenario: Missing provider credential

- **WHEN** the selected provider's credential is absent
- **THEN** the system reports a clear, actionable error and does not start the agent loop

#### Scenario: Provider failure

- **WHEN** the model provider returns an error or times out during a turn
- **THEN** the system reports the failure in the copilot pane, leaves the browser usable, and does not apply any cluster change

### Requirement: Agent acts only through typed tools

The system SHALL expose cluster operations to the model only as named tools with structured arguments, and SHALL NOT construct or execute shell command strings on behalf of the agent.

#### Scenario: Model requests a tool

- **WHEN** the model emits a tool call
- **THEN** the system dispatches it by name to the corresponding typed operation and returns the structured result to the model

#### Scenario: Unknown or malformed tool call

- **WHEN** the model names a tool that does not exist or supplies arguments that do not match the tool's schema
- **THEN** the system returns a descriptive error to the model as the tool result and does not execute any cluster operation

### Requirement: Two tool tiers with read auto-execution

The system SHALL classify tools as read or mutate, SHALL execute read tools automatically to gather context, and SHALL never execute a mutate tool without prior human approval.

#### Scenario: Read tool runs automatically

- **WHEN** the model requests a read tool such as listing resources, fetching a resource, reading logs, or reading events
- **THEN** the system executes it immediately and returns the result to the model

#### Scenario: Mutate tool is not auto-executed

- **WHEN** the model requests a mutate tool
- **THEN** the system does not apply the change and instead enters the approval flow

### Requirement: Curated reversible mutation set

In this change the system SHALL support only a closed set of reversible or non-destructive mutation tools: scaling a workload, restarting a workload's rollout, setting labels, and setting annotations. The system SHALL NOT support deletion, arbitrary apply or patch of supplied manifests, or image changes.

#### Scenario: Supported mutation

- **WHEN** the model proposes scaling, rollout restart, setting labels, or setting annotations
- **THEN** the proposal is eligible for the approval flow

#### Scenario: Unsupported mutation

- **WHEN** the model's request would delete a resource, apply an arbitrary manifest, or change a container image
- **THEN** the system refuses the action, explains that it is outside the supported set, and returns that explanation to the model as the tool result

### Requirement: Server-side validation before approval

Before presenting a mutating proposal to the human, the system SHALL validate it against the API server using a server-side dry-run. A dry-run that is rejected SHALL prevent the proposal from being shown as approvable.

#### Scenario: Valid proposal

- **WHEN** the proposed mutation passes server-side dry-run
- **THEN** the system presents the proposal to the human marked as validated

#### Scenario: Invalid proposal

- **WHEN** the proposed mutation is rejected by server-side dry-run
- **THEN** the system shows the server's rejection reason to the model and to the user, never presents it as approvable, and does not apply it

### Requirement: Explicit human approval gate

The system SHALL require an explicit human decision for every mutating proposal and SHALL show the verb, the target, the before and after state, the dry-run result, and whether the action is reversible. The system SHALL NOT proceed by timeout or default.

#### Scenario: Approve a proposal

- **WHEN** the human approves a validated proposal
- **THEN** the system applies the mutation, records the outcome, and returns the real result to the model

#### Scenario: Reject a proposal

- **WHEN** the human rejects a proposal
- **THEN** the system applies nothing and returns a "declined by user" result to the model so the loop can continue

#### Scenario: Approval waits indefinitely

- **WHEN** a proposal is awaiting a human decision
- **THEN** the agent loop remains blocked until the human responds, and does not apply the change on its own

#### Scenario: Apply fails after approval

- **WHEN** the mutation fails when applied even though the dry-run passed
- **THEN** the system reports the failure, records it, and returns the error to the model as the tool result

### Requirement: Browser focus is injected into each turn

The system SHALL include the browser's current focus — active namespace, selected resource type, and selected resource when present — in the context provided to the model on each turn.

#### Scenario: Copilot is aware of focus

- **WHEN** the user asks the copilot about the current view without restating the namespace or resource
- **THEN** the agent turn includes the browser's current focus and the model can act on it

### Requirement: Bounded agent turns

The system SHALL bound the number of model iterations and tool calls per user request and SHALL surface when the bound is reached rather than looping indefinitely.

#### Scenario: Iteration limit reached

- **WHEN** an agent turn reaches the configured iteration or tool-call limit
- **THEN** the system stops the turn, tells the user the limit was reached, and applies no unapproved change

### Requirement: Cluster authorization errors are surfaced

The system SHALL report authorization and permission failures returned by the cluster instead of presenting them as absence of resources.

#### Scenario: Caller lacks permission

- **WHEN** a tool call is rejected by the cluster because the caller lacks permission
- **THEN** the system reports the permission error to the model and the user

### Requirement: The copilot can read current resource usage

The system SHALL expose current CPU and memory usage to the model as a read tool covering nodes, namespaces, pods and the containers of a pod, returning the same readings the metrics screen shows. When usage cannot be obtained, the tool result SHALL say so as an error that states the cause and that usage is unknown, and SHALL NOT return zero or empty readings in its place.

#### Scenario: Model reads usage

- **WHEN** the model requests the metrics tool for a level of the hierarchy
- **THEN** the system executes it immediately as a read tool and returns usage set against capacity, requests and limits, with the age of the readings

#### Scenario: Metrics unavailable

- **WHEN** the model requests the metrics tool and the metrics source is absent, not answering, not permitted or too slow
- **THEN** the tool result is an error that names the cause, includes the server's message, and states that usage is unknown rather than zero

#### Scenario: Result too long

- **WHEN** a listing is too long to return whole
- **THEN** the result contains fewer rows and says how many of how many, and keeps every caveat about how to read them

#### Scenario: Readings are not current

- **WHEN** the readings returned are older than the latest attempt to refresh them, or their sample is old
- **THEN** the tool result says so

#### Scenario: Model is instructed how to use it

- **WHEN** a request is sent to the model
- **THEN** the instructions tell it to check load with the metrics tool rather than infer it, and not to describe a workload as idle when usage is unknown

### Requirement: The copilot can read the cluster's current findings

The system SHALL expose the cluster's current findings to the model as a read
tool, returning the same findings the health screen shows, with the class each
belongs to, the object it concerns, and the state of every class. When a class
was not watched, the result SHALL say so as a limitation of the answer and
SHALL NOT present it as an absence of problems.

#### Scenario: Model reads findings

- **WHEN** the model requests the findings tool
- **THEN** the system executes it immediately as a read tool and returns every
  finding with its class, object, reason and age, and the state of each class

#### Scenario: A class was not read

- **WHEN** the model requests findings and a class is forbidden, unavailable or
  not applicable
- **THEN** the result names that class as not watched with the cause, and does
  not say the cluster is healthy in that respect

#### Scenario: Nothing found

- **WHEN** every class was read and none produced a finding
- **THEN** the result says that each class was watched and that no findings were
  present, and does not claim anything beyond what was read

#### Scenario: Result too long

- **WHEN** the findings are too numerous to return whole
- **THEN** the result contains fewer findings, says how many of how many, and
  keeps every class's state

#### Scenario: Model is instructed how to use it

- **WHEN** a request is sent to the model
- **THEN** the instructions tell it to check current findings with this tool
  instead of inferring health from listings, and not to describe the cluster as
  healthy in a respect that was not read
