## MODIFIED Requirements

### Requirement: Expose current focus to the copilot

The system SHALL maintain and expose the current browser focus — the active namespace, the selected resource type, and the selected resource, when one is selected — so the copilot can include it in each agent turn. While the metrics screen is shown, the focus SHALL describe that screen: what it is listing and the node, namespace, pod or container that is selected.

#### Scenario: Focus changes are available to the copilot

- **WHEN** the user changes the namespace, resource type, or selected resource in the browser
- **THEN** the focus reported to the copilot reflects the change no later than the next agent turn

#### Scenario: No resource selected

- **WHEN** the browser has a namespace and type selected but no individual resource
- **THEN** the focus reported to the copilot describes the scope without claiming a selected resource

#### Scenario: Focus on the metrics screen

- **WHEN** the user is on the metrics screen with a row selected
- **THEN** the focus reported to the copilot names the metrics screen, what it is listing, and the selected node, namespace or pod, so that "this one" refers to that row

### Requirement: Keyboard-driven layout with copilot pane

The system SHALL provide keyboard navigation across namespaces, resource types, listings, detail views, and the metrics screen; SHALL present contextual key hints; and SHALL expose the copilot as a pane that can be shown, hidden, focused, and maximized from the keyboard without losing browser context.

#### Scenario: Navigate and go back

- **WHEN** the user moves through the interface and navigates back
- **THEN** the system follows the focus and returns through the previous views without losing context

#### Scenario: Toggle the copilot pane

- **WHEN** the user toggles the copilot pane
- **THEN** the pane is shown or hidden while the browser's namespace, type, and selection are preserved

#### Scenario: Maximize the copilot pane

- **WHEN** the user presses the maximize key while the browser has focus
- **THEN** the copilot is shown if it was hidden, occupies the entire body area instead of half of it, and takes focus, and the browser listing, detail, logs, and events panes are not rendered

#### Scenario: Restore the split from maximized

- **WHEN** the copilot pane is maximized and the user presses a key that returns to the browser
- **THEN** the pane returns to the split layout — copilot beside browser — and the browser regains focus, so no pane is focused while hidden

#### Scenario: Browser state survives maximize and restore

- **WHEN** the copilot pane is maximized and then restored
- **THEN** the browser's namespace, resource type, selection, filter, and open sub-view are unchanged

#### Scenario: Open and leave the metrics screen

- **WHEN** the user opens the metrics screen and later leaves it
- **THEN** the browser's namespace, resource type, selection and filter are as they were, and the key hints shown on the metrics screen are the ones that apply there

#### Scenario: Quit from any view

- **WHEN** the user presses the quit key
- **THEN** the application exits cleanly and restores the terminal

#### Scenario: Terminal too small

- **WHEN** the terminal is resized below the minimum usable dimensions
- **THEN** the system shows a readable message instead of a broken layout
