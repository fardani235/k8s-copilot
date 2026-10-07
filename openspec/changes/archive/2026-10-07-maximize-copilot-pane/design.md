## Context

The TUI body is two panes joined horizontally (`internal/tui/model.go`,
`View`): the browser on the left, and — when `copilotOpen` — the copilot on the
right at `clamp(width*42/100, 34, 80)` (`copilotWidth`). `layout()` sizes each
pane's viewport from `browserInner`/`copilotInner`, and `pane` records which
pane owns the keyboard. Global browser keys (`c`, `tab`, `p`, `?`, `A`) are
handled before pane dispatch; once `pane == paneCopilot`, `onCopilotKey`
handles `tab`, `esc`, scrolling, `ctrl+l`, and `enter`, and sends everything
else to the text input. The approval dialog takes over the whole screen
independently and is unaffected.

The maximize requirement (see `specs/resource-browser/spec.md`) is a display
and focus change only. It must not alter what focus is published to the agent.

## Goals / Non-Goals

**Goals:**

- A single `m` keypress from the browser gives the copilot the whole body and
  makes it immediately usable.
- Leaving the copilot returns to the split with the browser focused; browser
  context is untouched.
- The change stays local to the `tui` package and to the existing layout and
  focus code paths.

**Non-Goals:**

- No change to the cursor, selection, views, filter, or the focus published to
  the agent.
- No configurable key bindings and no persisted layout preference.
- No maximize for the browser pane.
- No change to the approval dialog's full-screen takeover.

## Decisions

**A dedicated `copilotMax` boolean, not a width override.** Making
`copilotWidth` return the full width would leave the browser one column wide
and still render it. A separate state instead lets `View` skip the browser
entirely and hand the whole body to the copilot. It is orthogonal to
`copilotOpen`: maximize implies open, and restore leaves the pane open in the
split. Alternative considered: folding maximize into `copilotOpen` with a
second enum value — rejected as less readable and harder to keep the split
transition correct.

**Maximize takes focus; `esc`/`tab` restore.** Once the copilot input is
focused, `m` is an ordinary character, so it cannot double as the way back
without swallowing typed text. Maximize therefore moves focus to the copilot,
and the existing "leave the copilot" keys restore the split. This guarantees
there is never a state in which a hidden browser holds the keyboard.
Alternative considered: keep browser focus while maximized so `m` toggles both
ways — rejected because keystrokes would then drive an invisible pane.

**Restore returns to the visible split, not to a previously hidden state.** If
the copilot was hidden, maximizing opens it; restoring leaves it in the split.
The user just used the copilot, and the split is the state in which the pane is
visible and usable. This is simpler than remembering a prior visibility bit,
and it matches the user's stated expectation that restore returns to the split.

**Layout is recomputed centrally.** `layout()` already recomputes every
viewport. Maximize and restore flip the boolean and call `layout()`, so
`browserInner`, `copilotInner`, and `listRows` stay consistent. No new sizing
path is introduced.

**Hint text.** The browser footer gains `m maximize`; while maximized, the
copilot footer keeps its existing `tab/esc browser` hints, which are exactly
the way back. `helpText()` and the README are updated to name the key.

## Risks / Trade-offs

- [The way back is not the same key as the way in] → The footer already labels
  `tab/esc browser`; add `m maximize` to the browser hints, and name `m` in
  `helpText()` and the README.
- [Reflowing viewports on maximize can lose scroll position] → `renderChat`
  already jumps to the transcript bottom and the browser list reloads; this
  matches the existing resize behavior. Acceptable.
- [The maximize key is pressed when the copilot is unavailable] → Maximize
  still works: it shows the copilot pane with its existing "copilot is not
  available" message, exactly like `c`.
- [Maximized state left on when a proposal dialog appears] → The dialog
  renders over everything and reveals the underlying layout unchanged on
  close; there is no new interaction with the approval flow.

## Migration Plan

None. Single binary, no stored state, no API or configuration change. Rollback
is reverting the commit; the feature has no external side effects.

## Open Questions

None.
