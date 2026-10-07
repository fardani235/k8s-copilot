## Why

The copilot shares the body with the browser and is capped at 42% of the
terminal width (`copilotWidth`). Long answers, multi-line tool output, and
YAML-heavy explanations are cramped into that column, and there is no way to
give the copilot the whole screen without losing the split entirely. A user
reading an investigation wants to let the copilot take the full body for a
moment, then drop back to the split without losing where they were in the
browser.

## What Changes

- Add a maximize/restore behavior for the copilot pane, entered with `m` from
  the browser (alongside the existing `c` show/hide and `tab` focus keys).
- When maximized, the copilot occupies the entire body area and the browser
  pane is not rendered; the header (context, namespace, type) and the footer
  key hints remain, so browser context stays readable.
- Maximizing opens the copilot if it was hidden and moves focus to it (input
  focused). Restoring returns to the split layout with focus on the browser.
- Because `m` is an ordinary character while typing in the copilot, the way
  back is the existing "leave the copilot" keys: `esc` or `tab` restore the
  split instead of leaving a focused but invisible browser.
- Browser state — namespace, resource type, selection, filter, and the open
  sub-view — is preserved across maximize/restore.
- The key hints name the maximize binding and the way back.

## Capabilities

### New Capabilities
<!-- None: this extends the existing terminal layout behavior. -->

### Modified Capabilities

- `resource-browser`: The "Keyboard-driven layout with copilot pane"
  requirement gains maximizing and restoring the copilot pane, and the
  guarantee that browser context survives the transition.

## Impact

- `internal/tui/model.go`: a maximized state field; `m` handling in the global
  browser keys; focus transitions; layout math (`View`, `layout`,
  `browserInner`, `copilotInner`, `copilotWidth`, `listRows`); footer hints;
  help text hook-in.
- `internal/tui/copilot.go`: `esc` and `tab` restore the split when maximized.
- `internal/tui/views.go` (`helpText`): document the `m` key.
- `README.md`: add `m` to the copilot key documentation.
- Tests: `internal/tui/tui_test.go` gains coverage for maximize, restore, and
  browser-state preservation.
- No new dependencies. No cluster, configuration, approval, audit, or LLM
  behavior changes. The maximized layout is display-only and does not touch
  what the copilot is told about the browser focus.
