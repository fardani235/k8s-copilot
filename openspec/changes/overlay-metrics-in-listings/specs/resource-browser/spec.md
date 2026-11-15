# Spec Delta

## ADDED Requirements

### Requirement: Show usage beside the pods and nodes listings

The pods listing SHALL show the usage columns the metrics model provides after the listing's own columns, and the nodes listing likewise, without changing the listed resources, their order, the selection or the filter.

#### Scenario: Columns appear after the listing's own

- **WHEN** usage is available for the current pods or nodes listing
- **THEN** the listing's own columns stay first and in place, and the usage columns follow them

#### Scenario: No usage is not an empty listing

- **WHEN** usage cannot be shown
- **THEN** the listing still shows every resource it would have shown, with one line saying why the usage columns are absent and no placeholder column in their place

#### Scenario: Selection and refresh survive

- **WHEN** the listing refreshes while a row is selected
- **THEN** the selection stays on the same resource, and the usage columns refresh with the listing

#### Scenario: Filtering is unchanged

- **WHEN** the user filters the listing
- **THEN** the filter matches the resource's own fields as before, and a usage figure does not make an otherwise non-matching row match

#### Scenario: A narrow pane

- **WHEN** the pane is too narrow for every column
- **THEN** usage columns give way before the listing's own columns, so that names, status and age remain, and the listing never draws outside its pane

#### Scenario: Cluster-scoped nodes

- **WHEN** the user lists nodes, which are cluster-scoped
- **THEN** the nodes listing shows its usage columns without implying a namespace scope
