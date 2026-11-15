# Spec Delta

## ADDED Requirements

### Requirement: The copilot's listings carry the same usage overlay

When the copilot lists pods or nodes, the result SHALL include the same usage columns the browser listing shows, from the same readings, and SHALL state in the same words when usage is not available and is therefore absent.

#### Scenario: Listing pods carries usage

- **WHEN** the copilot lists pods and usage is available
- **THEN** each row in the result shows the same CPU, memory and bound-share columns the browser listing shows

#### Scenario: Listing nodes carries usage

- **WHEN** the copilot lists nodes and usage is available
- **THEN** each row shows the same CPU, memory and allocatable-share columns the browser listing shows

#### Scenario: Usage unavailable

- **WHEN** the copilot lists pods or nodes and the readings cannot be obtained
- **THEN** the result has no usage columns, states why in the same words the browser listing uses, and says that usage is unknown rather than zero

#### Scenario: A row without a sample

- **WHEN** one listed row has no reading while the column has values elsewhere
- **THEN** the result shows the unknown marker for that row and does not present it as zero

#### Scenario: One read

- **WHEN** the copilot lists pods or nodes while the browser or the metrics screen holds recent readings
- **THEN** the columns come from those readings and no further metrics request is made
