# Spec Delta

## ADDED Requirements

### Requirement: The copilot transcript can be scrolled

The system SHALL let the user scroll the copilot transcript with the mouse wheel while the pointer is over the pane, and with keys while the pane has focus: up/down one line, pgup/pgdown one page, ctrl+u/ctrl+d half a page, ctrl+home/ctrl+end to either end. It SHALL show the visible range while the transcript is longer than the pane.

#### Scenario: Mouse wheel over the copilot pane

- **WHEN** the pointer is over the copilot pane and the user turns the wheel up or down
- **THEN** the transcript's visible lines move in that direction, and no other view changes

#### Scenario: Keys while the copilot has focus

- **WHEN** the copilot pane has focus and the user presses up, down, pgup, pgdown, ctrl+u, ctrl+d, ctrl+home or ctrl+end
- **THEN** the transcript moves by the corresponding amount — one line, one page, half a page, or to the end — and the input's own editing keys keep working

#### Scenario: The visible range is shown

- **WHEN** the transcript is longer than the pane's height
- **THEN** the pane shows the visible lines and the total, and updates that range as the view moves; when the transcript fits, no range is shown

#### Scenario: Reading older output is not interrupted

- **WHEN** the user has scrolled away from the end and new copilot output arrives
- **THEN** the view stays where it is, and it returns to the end only when the user sends a message

#### Scenario: Wheel outside the copilot pane

- **WHEN** the pointer is over the browser, or the copilot pane is hidden
- **THEN** the copilot transcript does not move
