## ADDED Requirements

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
