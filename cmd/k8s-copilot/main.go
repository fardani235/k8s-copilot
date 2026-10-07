// Command k8s-copilot is a terminal Kubernetes browser with an AI copilot that
// investigates freely and changes nothing without explicit approval.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/fardani235/k8s-copilot/internal/agent"
	"github.com/fardani235/k8s-copilot/internal/approval"
	"github.com/fardani235/k8s-copilot/internal/audit"
	"github.com/fardani235/k8s-copilot/internal/config"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/llm"
	"github.com/fardani235/k8s-copilot/internal/tools"
	"github.com/fardani235/k8s-copilot/internal/tui"
)

// version is set at build time: -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	inv, err := config.Parse(args, getenv, stderr)
	if errors.Is(err, config.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "k8s-copilot:", err)
		return 2
	}
	cfg := inv.Config

	switch inv.Command {
	case "version":
		fmt.Fprintln(stdout, "k8s-copilot", version)
		return 0
	case "config":
		fmt.Fprint(stdout, cfg.Describe(getenv))
		return 0
	case "audit-verify", "audit-show":
		return auditCommand(inv.Command, cfg.AuditFile, stdout, stderr)
	}

	// client-go writes warnings to stderr, which would scribble over the UI.
	klog.SetOutput(io.Discard)
	klog.LogToStderr(false)
	rest.SetDefaultWarningHandler(rest.NoWarnings{})

	cluster, err := kube.Connect(context.Background(), kube.ConnectOptions{
		Kubeconfig: cfg.Kubeconfig, Context: cfg.Context, UserAgent: "k8s-copilot/" + version,
	})
	if err != nil {
		fmt.Fprintln(stderr, "k8s-copilot:", err)
		return 1
	}

	deps := tui.Deps{
		Cluster: cluster, Focus: &tui.FocusStore{}, AuditPath: cfg.AuditFile,
		Refresh: cfg.RefreshInterval, Namespace: cfg.Namespace, Version: version,
	}

	// The audit trail. If it cannot be opened the copilot still investigates,
	// but every change proposal is refused: no record, no change.
	trail, err := audit.Open(cfg.AuditFile)
	switch {
	case err != nil:
		deps.AuditNote = fmt.Sprintf("%v. The copilot can still investigate, but it will not propose any change until the audit trail works.", err)
	case !trail.Initial().OK():
		deps.AuditNote = fmt.Sprintf("The existing audit trail at %s fails its integrity check (%d problem(s)): entries were changed or removed outside k8s-copilot. Run `k8s-copilot audit verify` for details. New entries are still appended.",
			cfg.AuditFile, len(trail.Initial().Problems))
	}
	if trail != nil {
		defer trail.Close()
	}

	// The copilot. Any problem here leaves a working browser and a pane that
	// says what to fix.
	pcfg, err := cfg.ProviderConfig(getenv)
	var provider llm.Provider
	if err == nil {
		provider, err = llm.New(pcfg)
	}
	if err != nil {
		deps.AgentErr = err.Error()
	} else {
		deps.Gate = approval.NewGate()
		registry := tools.NewRegistry(cluster, tools.Options{
			MaxResultBytes: cfg.MaxResultBytes, MaxReplicas: cfg.MaxReplicas,
			ProtectedNamespaces: cfg.ProtectedNamespaces, RedactSecrets: cfg.RedactSecrets,
		})
		deps.Agent = agent.New(agent.Config{
			Provider: provider, Registry: registry, Gate: deps.Gate, Audit: trail,
			Limits:   agent.Limits{MaxIterations: cfg.MaxIterations, MaxToolCalls: cfg.MaxToolCalls},
			Focus:    deps.Focus.Get,
			Info:     cluster.Info,
			Approver: approver(),
			Session:  randomID(),
		})
	}

	model := tui.New(deps)
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	// Let a proposal that was still waiting be recorded as cancelled.
	model.Shutdown()
	if err != nil {
		fmt.Fprintln(stderr, "k8s-copilot:", err)
		if strings.Contains(err.Error(), "TTY") {
			fmt.Fprintln(stderr, "k8s-copilot is an interactive program and needs a terminal. (`k8s-copilot audit show`, `audit verify` and `config` work without one.)")
		}
		return 1
	}
	return 0
}

func auditCommand(cmd, path string, stdout, stderr io.Writer) int {
	rep, err := audit.Verify(path)
	if err != nil {
		fmt.Fprintln(stderr, "k8s-copilot:", err)
		return 1
	}
	if cmd == "audit-show" {
		fmt.Fprint(stdout, tui.FormatAudit(path, rep))
	} else {
		fmt.Fprintf(stdout, "%s: %d entries\n", path, len(rep.Entries))
		for _, n := range rep.Notes {
			fmt.Fprintln(stdout, "note:", n)
		}
		if rep.OK() {
			fmt.Fprintln(stdout, "OK: every entry matches its hash and the chain is intact.")
		} else {
			fmt.Fprintln(stdout, "FAILED: the audit trail has been altered or is damaged:")
			for _, p := range rep.Problems {
				fmt.Fprintln(stdout, "  -", p)
			}
		}
	}
	if !rep.OK() {
		return 1
	}
	return 0
}

// approver names who is at the keyboard, for the audit entry.
func approver() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		name += "@" + host
	}
	return name
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "session"
	}
	return hex.EncodeToString(b)
}
