# audit-trail Specification

## Purpose
Provide a durable, append-only record of every gated action so a team can reconstruct what the agent proposed, what a human approved, and what actually changed.

## Requirements

### Requirement: Record every gated action

The system SHALL record a durable entry for every mutating proposal, whether it was approved, rejected, or failed, capturing at minimum: the time, the user intent that led to it, the tool name, the target resource, the structured arguments, the server-side dry-run result, the human decision, and the final outcome.

#### Scenario: Approved action is recorded

- **WHEN** a mutating proposal is approved and applied
- **THEN** an entry records the intent, tool, target, arguments, dry-run result, approver decision, and the outcome of the apply

#### Scenario: Rejected action is recorded

- **WHEN** a mutating proposal is rejected by the human
- **THEN** an entry records the proposal and the rejection decision, including that no change was applied

#### Scenario: Failed action is recorded

- **WHEN** a mutating proposal passes dry-run and approval but the apply fails
- **THEN** an entry records the failure and the error

#### Scenario: Invalid proposal is recorded

- **WHEN** a mutating proposal is rejected by server-side dry-run
- **THEN** an entry records the proposal and the rejection reason

### Requirement: Append-only integrity

The audit trail SHALL be append-only from the application's perspective: entries SHALL NOT be modified or deleted by the application after being written, and the trail SHALL use a format that permits detecting tampering or truncation.

#### Scenario: Entries are immutable

- **WHEN** any entry has been written
- **THEN** no application operation modifies or removes it

#### Scenario: Tampering is detectable

- **WHEN** an entry's content or ordering is altered outside the application
- **THEN** the integrity mechanism allows the alteration to be detected

### Requirement: Durable across restarts

The audit trail SHALL persist to local storage and SHALL retain entries across application restarts.

#### Scenario: Entries survive restart

- **WHEN** the user restarts the application after recording actions
- **THEN** the previously recorded entries are still present

#### Scenario: Storage unavailable

- **WHEN** the audit trail cannot be written
- **THEN** the system reports the failure and does not treat the action as successfully audited
