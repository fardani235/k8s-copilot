## ADDED Requirements

### Requirement: Reach the health screen and return from it

The system SHALL provide one key, available from any browser view, that opens a
health screen listing the cluster's current findings and what is not being
watched. Opening it SHALL NOT change the browser's namespace, resource type,
selection or filter, and leaving it SHALL return to the view it was opened from
with those unchanged.

#### Scenario: Open the health screen

- **WHEN** the user presses the health key in any browser view
- **THEN** the system shows the cluster's findings grouped by class, the state
  of each class, and the age of the reading

#### Scenario: Leave the health screen

- **WHEN** the user leaves the health screen
- **THEN** the system returns to the browser view it was opened from with the
  namespace, resource type, selection and filter as they were

#### Scenario: Open logs, events or detail from a finding

- **WHEN** the user asks for the logs, events or detail of the selected finding
- **THEN** the system opens the corresponding browser view for that object and
  returns to the health screen with the same finding selected when it is
  dismissed

#### Scenario: Key hints match the screen

- **WHEN** the health screen is shown
- **THEN** the key hints describe the keys that apply there rather than the
  listing's keys

### Requirement: The focus describes the health screen

While the health screen is shown, the system SHALL report that screen as the
browser focus — that it is the health screen, what it is listing, and the
selected finding — so the copilot resolves "this one" to that finding.

#### Scenario: A finding is selected

- **WHEN** the health screen is shown with a finding selected and the user asks
  the copilot about it
- **THEN** the focus reported to the copilot names the health screen, the
  finding's class, and the object it concerns

#### Scenario: No finding selected

- **WHEN** the health screen is shown with no finding selected
- **THEN** the focus describes the screen and its classes without claiming a
  selected object

#### Scenario: Leaving the screen restores the browser focus

- **WHEN** the user leaves the health screen
- **THEN** the focus reported to the copilot describes the browser view again
