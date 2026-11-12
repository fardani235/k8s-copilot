# Spec Delta

## Purpose

Tell an operator what is wrong in the cluster right now — and, just as plainly,
which conditions were not read — using only the cluster's existing read-only
API, without installing, configuring or changing anything.

## ADDED Requirements

### Requirement: Classify workload conditions

The system SHALL classify the current conditions of pods cluster-wide into
findings: a container that is crash-looping or restarting repeatedly, a
container whose image cannot be pulled, and a pod that is running but not
ready. Each finding SHALL name the pod, its namespace, the affected container,
and what was observed.

#### Scenario: Crash-looping container

- **WHEN** a pod has a container waiting in CrashLoopBackOff, or a container has
  restarted more times than the threshold the system states
- **THEN** one finding names the pod, its namespace and container, its restart
  count, and how long the condition has held

#### Scenario: Image cannot be pulled

- **WHEN** a container is waiting because its image cannot be pulled
- **THEN** a finding names the pod, the container, the image, and the reason the
  cluster gives

#### Scenario: Pod not ready

- **WHEN** a pod is running but none of its containers is ready for longer than
  the grace period the system states
- **THEN** a finding names the pod and says which containers are not ready

#### Scenario: No workload condition

- **WHEN** every pod the caller can read is ready and not restarting
- **THEN** the workload class reports that it is watched and has no findings

### Requirement: Classify node conditions

The system SHALL classify the current conditions of nodes into findings: a node
that is NotReady, and a node that reports memory pressure or disk pressure.
Each finding SHALL name the node and the condition it reports.

#### Scenario: Node not ready

- **WHEN** a node's Ready condition is false
- **THEN** a finding names the node, the reason the cluster gives, and how long
  the condition has held

#### Scenario: Node under pressure

- **WHEN** a node reports memory pressure or disk pressure
- **THEN** a finding names the node and which pressure it reports, one finding
  per pressure

#### Scenario: Healthy node

- **WHEN** a node is Ready and reports neither pressure
- **THEN** it produces no finding

### Requirement: Classify rollouts that are not progressing

The system SHALL classify Deployments, StatefulSets and DaemonSets whose
rollout is not progressing into findings, naming the workload, its namespace,
and the reason the cluster gives. A rollout that has completed, or one that is
deliberately paused, SHALL NOT be reported as failing.

#### Scenario: Deployment not progressing

- **WHEN** a Deployment reports that it is not progressing
- **THEN** one finding names the Deployment, its namespace, the replicas that
  are available against those desired, and the cluster's reason

#### Scenario: StatefulSet or DaemonSet not progressing

- **WHEN** a StatefulSet or DaemonSet reports that it is not progressing
- **THEN** one finding names it, its namespace, and the cluster's reason

#### Scenario: Paused rollout

- **WHEN** a workload's rollout is paused deliberately
- **THEN** no finding is produced for it

#### Scenario: Completed rollout

- **WHEN** every desired replica is updated and available
- **THEN** no finding is produced for that workload

### Requirement: Report recent Warning events

The system SHALL list recent Warning events cluster-wide, newest first, within
a window it states, and SHALL group repeated identical warnings into one entry
with a count rather than one row per occurrence.

#### Scenario: Warning events are listed

- **WHEN** Warning events occurred within the window
- **THEN** each distinct warning names the object it concerns, the reason, the
  message, when it last occurred, and how many times it repeated

#### Scenario: No Warning events

- **WHEN** no Warning events occurred within the window
- **THEN** the events class reports that it is watched and that the window was
  clear, and still states the window

#### Scenario: Window is stated

- **WHEN** Warning events are listed or reported clear
- **THEN** the window those events were read over is shown

### Requirement: State what is and is not being watched

Each class of findings SHALL carry whether it is watched, forbidden,
unavailable or not applicable, and the reason. A class that is not watched
SHALL be shown as not watched on the screen and to the copilot, SHALL produce
no findings, and SHALL NOT be described as healthy or clear.

#### Scenario: A class the caller may not read

- **WHEN** the cluster refuses a read a class depends on
- **THEN** that class is shown as forbidden, with the server's reason, and
  produces no findings

#### Scenario: Cluster-wide read refused, namespace readable

- **WHEN** the caller may not read a class across the cluster but may in the
  browser's current namespace
- **THEN** that class is read for that namespace and says it covers only that
  namespace and why

#### Scenario: Metrics source absent

- **WHEN** the metrics API is not served, not answering, or not permitted
- **THEN** the usage class is shown as not applicable or unavailable with the
  cause, produces no findings, and is never described as nothing being hot

#### Scenario: Everything watched and clear

- **WHEN** every class was read and none produced a finding
- **THEN** the screen says that each class is watched and that there are no
  findings, and does not imply more than it read

### Requirement: Report usage at a hard bound

When the metrics source is available, the system SHALL classify a node's usage
against its allocatable resources, and a container's usage against its limit,
into findings using the same threshold the metrics screen uses. Usage below
that threshold SHALL NOT be reported.

#### Scenario: Node at a hard bound

- **WHEN** a node's usage reaches the threshold share of its allocatable
  resources
- **THEN** a finding names the node, the resource, the usage and the bound

#### Scenario: Container near its limit

- **WHEN** a container's usage reaches the threshold share of its limit
- **THEN** a finding names the pod, the container, the resource and the limit

#### Scenario: Below the threshold

- **WHEN** usage is known and below the threshold
- **THEN** no finding is produced for it

### Requirement: Read-only, with no new permissions

The system SHALL obtain every finding with read-only requests made with the
caller's existing credentials. It SHALL NOT create, modify or delete any
cluster object, install or configure anything, or require an additional
permission, including when reads fail.

#### Scenario: Only reads are sent

- **WHEN** findings are obtained, successfully or not
- **THEN** no create, update, patch or delete request is sent

#### Scenario: Nothing is installed or configured

- **WHEN** a class cannot be read
- **THEN** the system reports that state and does not install, configure or ask
  the user to install anything

### Requirement: Live while visible, with the age of the reading

The system SHALL refresh its findings while they are visible, at the configured
refresh interval or a floor it states, SHALL state how old the reading is, and
SHALL NOT present findings from an earlier read as current. It SHALL NOT read
while it is not visible, and SHALL NOT start a read while one is outstanding.

#### Scenario: Periodic refresh

- **WHEN** the health screen is visible and the refresh interval elapses
- **THEN** the classes are read again and the screen updates, keeping the
  selected row selected when it is still present

#### Scenario: Not visible

- **WHEN** the health screen is not visible
- **THEN** no read is started for it

#### Scenario: Age is shown

- **WHEN** findings are shown
- **THEN** the screen shows how old the oldest read behind them is, and that age
  keeps increasing while nothing is pressed

#### Scenario: Refresh fails after a successful read

- **WHEN** a refresh obtains nothing after an earlier read succeeded
- **THEN** the earlier findings are no longer presented as current, and the
  screen says why nothing could be read

#### Scenario: Slow source

- **WHEN** a read does not answer promptly
- **THEN** the interface stays responsive, the screen says it is reading rather
  than showing an empty result, and the read is abandoned after a bounded time
  with a stated reason

### Requirement: One set of findings for the screen and the copilot

The system SHALL derive what the health screen shows and what the copilot is
given from the same classification of the same read, so that the two cannot
differ in which conditions are reported, how they are described, or why a class
is not watched.

#### Scenario: Same findings

- **WHEN** the copilot is given findings while the health screen is showing
  recent ones
- **THEN** the copilot is given those findings rather than a separate read

#### Scenario: The copilot read first

- **WHEN** the copilot reads findings and the health screen is then opened within
  the reuse window
- **THEN** the screen shows the findings the copilot was given, without a
  further read

#### Scenario: Same explanation

- **WHEN** a class is not watched
- **THEN** the copilot is told so in the same terms the screen uses

### Requirement: Degrade without breaking

The health screen SHALL remain readable and SHALL stay inside its pane when the
terminal or the browser pane is small, keeping each finding's object, class and
reason, and cutting longer text with a marker rather than overflowing.

#### Scenario: Narrow or short pane

- **WHEN** the terminal or the browser pane is narrow or short
- **THEN** the screen drops less important detail, keeps each finding's object
  and class, and never draws outside its pane

#### Scenario: Below the minimum

- **WHEN** the terminal is resized below the minimum usable dimensions while the
  health screen is open
- **THEN** the system shows its usual readable message instead of a broken
  layout, and restores the screen when the terminal is enlarged
