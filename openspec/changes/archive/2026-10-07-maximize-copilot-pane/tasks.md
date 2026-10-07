## 1. Layout state

- [x] 1.1 Add a `copilotMax bool` field to `Model` in `internal/tui/model.go` and make the layout helpers (`copilotWidth`, `browserInner`, `copilotInner`, `listRows`) account for it so that when maximized the copilot receives the full body width; verify `go build ./...` succeeds
- [x] 1.2 Update `View` in `internal/tui/model.go` to render only the copilot pane at the full body size when maximized, keeping the header and footer; verify a test shows the copilot title and no browser listing text while maximized

## 2. Key handling and focus

- [x] 2.1 Handle `m` in the global browser keys (beside `c` and `tab`) to enter the maximized state, ensure the copilot is open, and focus its input; verify a test that pressing `m` from the list renders the full-body copilot and leaves the browser's namespace, type, and selection unchanged
- [x] 2.2 Make `esc` and `tab` in `onCopilotKey` (`internal/tui/copilot.go`) clear the maximized state when set, restoring the split and browser focus; verify tests that after `m` then `esc` (and after `m` then `tab`) both panes render again and `pane == paneBrowser`

## 3. Hints and help

- [x] 3.1 Add `m maximize` to the browser footer hints in `viewFooter` and name `m` in `helpText()` (`internal/tui/views.go`); verify `go test ./internal/tui/...` passes and both strings are present
- [x] 3.2 Add `m` to the copilot key documentation in `README.md`; verify the file names the maximize key

## 4. Verification

- [x] 4.1 Add tests in `internal/tui/tui_test.go` covering maximize-and-focus, restore via `esc` and via `tab`, browser state preservation (namespace, type, selection, filter, open sub-view), and maximizing while the copilot is unavailable; verify `go test ./internal/tui/...` passes
- [x] 4.2 Run `make check` (vet plus the full test suite) and confirm it passes
