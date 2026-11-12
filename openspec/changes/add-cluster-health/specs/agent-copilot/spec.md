## ADDED Requirements

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
