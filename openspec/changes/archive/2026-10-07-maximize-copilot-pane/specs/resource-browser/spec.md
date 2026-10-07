## MODIFIED Requirements

### Requirement: Keyboard-driven layout with copilot pane

The system SHALL provide keyboard navigation across namespaces, resource types, listings, and detail views; SHALL present contextual key hints; and SHALL expose the copilot as a pane that can be shown, hidden, focused, and maximized from the keyboard without losing browser context.

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

#### Scenario: Quit from any view

- **WHEN** the user presses the quit key
- **THEN** the application exits cleanly and restores the terminal

#### Scenario: Terminal too small

- **WHEN** the terminal is resized below the minimum usable dimensions
- **THEN** the system shows a readable message instead of a broken layout
