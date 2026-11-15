# Spec Delta

## ADDED Requirements

### Requirement: Overlay current usage on the ordinary pods and nodes listings

The system SHALL add current CPU and memory columns to the ordinary pods and nodes listings, taken from the same shared readings the metrics screen uses, without a separate read or a separate computation.

#### Scenario: Pods carry usage

- **WHEN** the metrics API answered for the listing's scope and the ordinary listing shows pods
- **THEN** each row shows its current CPU and memory, and its usage as a share of the requests and limits the metrics screen shows for it

#### Scenario: Nodes carry usage

- **WHEN** the metrics API answered and the ordinary listing shows nodes
- **THEN** each row shows its current CPU and memory, and its usage as a share of the node's allocatable resources

#### Scenario: Recent readings are reused

- **WHEN** the ordinary listing is shown while the metrics screen or the copilot holds readings no older than the freshness window
- **THEN** the columns are taken from those readings and no further metrics request is made

#### Scenario: Same values as the metrics screen

- **WHEN** the same node or pod is shown in both the ordinary listing and the metrics screen
- **THEN** its usage, its percentage of the same bound, and its unknown marker are equal

### Requirement: Add no usage columns when the readings are not there

When the readings behind the overlay cannot be obtained, the system SHALL add no usage column at all — not a column of the unknown marker, not a column of zeros, not an empty one — and SHALL state in one line why.

#### Scenario: Metrics API not served

- **WHEN** the cluster does not serve the metrics API
- **THEN** the listing shows its own columns only, and says that metrics are not available on this cluster

#### Scenario: Metrics API not answering

- **WHEN** the metrics API is registered but its backend does not answer
- **THEN** the listing shows its own columns only, and says that the metrics API is not answering

#### Scenario: Not permitted or not authenticated

- **WHEN** the cluster refuses the caller's metrics request or rejects the caller's credentials
- **THEN** the listing shows its own columns only, and says which failure this is, in the server's own words when there are any

#### Scenario: Did not answer in time

- **WHEN** the metrics request does not complete in time
- **THEN** the listing shows its own columns only, and says that the readings did not arrive in time

#### Scenario: Readings do not cover the listing

- **WHEN** the readings cover one namespace but the listing covers all namespaces
- **THEN** no usage columns are added, and the listing says which namespace the readings cover

### Requirement: Add a usage column only when some row has a value for it

The system SHALL add a usage column only if at least one listed row has a known value for it. A row without a reading SHALL show the unknown marker, and a measured zero SHALL be shown as zero.

#### Scenario: One row without a sample

- **WHEN** the metrics API returned no sample for a listed node or pod
- **THEN** that row shows the unknown marker in the usage columns and is left out of the percentages and totals that cover it

#### Scenario: A measured zero

- **WHEN** the metrics API reports zero usage for a listed row
- **THEN** the row shows zero

#### Scenario: Every cell would be unknown

- **WHEN** no listed row has a known value for a column that would otherwise be added
- **THEN** that column is not added, rather than drawn as a column of the unknown marker

#### Scenario: Usage known, bound not

- **WHEN** usage can be read but the bound a percentage needs cannot
- **THEN** the usage columns are shown, the affected percentage columns are not added, and the listing says why

### Requirement: The listing states how old its usage is

While usage columns are shown, the listing SHALL state the age of the readings, and SHALL mark them when they are old or when a failed refresh has left earlier readings standing in.

#### Scenario: Recent readings

- **WHEN** usage columns are shown from a recent sample
- **THEN** the listing states how old the sample is

#### Scenario: Age keeps counting

- **WHEN** the listing is left on screen without being refreshed, including with periodic refresh switched off
- **THEN** the age it states keeps increasing, and the readings are flagged once they are old

#### Scenario: A failed refresh

- **WHEN** a refresh obtains no usage after an earlier one did
- **THEN** the columns remain, marked as not current together with the cause, and after the hold time the unavailable state is shown instead

### Requirement: The browser listing and the copilot's listing are the same table

The usage columns the ordinary listing shows and the ones the copilot's listing returns SHALL come from the same readings and the same computation, so that for the same row and column the value, the percentage, the unknown marker and the stated absence are equal.

#### Scenario: Browser and copilot agree

- **WHEN** the copilot lists pods or nodes while the browser shows the same listing
- **THEN** every value and percentage in the copilot's result equals the browser's for the same row and column

#### Scenario: Absence agrees

- **WHEN** usage cannot be shown
- **THEN** the copilot's listing has no usage columns either, and states the same reason in the same words as the browser
