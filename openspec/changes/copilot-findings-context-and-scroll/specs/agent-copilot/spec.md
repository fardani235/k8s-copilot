# Spec Delta

## ADDED Requirements

### Requirement: Current findings are in context on every turn

The system SHALL include in every copilot request a bounded snapshot of the current classified findings, rendered from the same report the health screen shows, with the reading's age and the state of every class. When the snapshot cannot be taken the context SHALL say so. The snapshot SHALL NOT replace the findings tool.

#### Scenario: A health question needs no discovery pass

- **WHEN** the user asks what is broken, what is wrong or whether the cluster is healthy, and a health snapshot was included
- **THEN** the request context contains the findings and every class's state, and the system's instructions tell the model to answer from that snapshot rather than listing resources to discover health

#### Scenario: The snapshot is too long for the budget

- **WHEN** the findings do not all fit in the snapshot's size bound
- **THEN** the snapshot keeps every class's state, says how many findings of each class were left out, and tells the model to call the findings tool for the rest

#### Scenario: The snapshot cannot be taken

- **WHEN** the health read fails or exceeds its bound
- **THEN** the request still proceeds with a stated "not available" note in its context, and the instructions still forbid inferring health from listings or presenting an unwatched class as healthy

#### Scenario: The snapshot carries the reading's age

- **WHEN** a snapshot is included in a request
- **THEN** it states how old the oldest read behind it is, so the model can say the findings are not current instead of implying they were read at that instant
