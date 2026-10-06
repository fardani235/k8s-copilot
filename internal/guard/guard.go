// Package guard holds no code. Its tests read the module's source and fail
// if a structural safety property stops holding:
//
//   - nothing can run a subprocess (so no kubectl, no shell);
//   - only internal/kube talks to the Kubernetes API;
//   - inside internal/kube, the only write call is the one in write.go, and
//     nothing creates or deletes;
//   - only the agent's gated path calls Plan.Apply;
//   - the audit package cannot rewrite, truncate or remove the log.
//
// These are the properties a reviewer would otherwise have to re-check by
// hand on every change.
package guard
