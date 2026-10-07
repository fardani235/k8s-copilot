package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fardani235/k8s-copilot/internal/audit"
)

func exec(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["K8S_COPILOT_CONFIG"]; !ok {
		p := filepath.Join(t.TempDir(), "empty.yaml")
		os.WriteFile(p, []byte("{}\n"), 0o600)
		env["K8S_COPILOT_CONFIG"] = p
	}
	code = run(args, func(k string) string { return env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

// 1.3: start-up failures are reported clearly, exit non-zero, and never get
// as far as starting the UI.
func TestStartupFailuresExitNonZero(t *testing.T) {
	t.Run("no kubeconfig", func(t *testing.T) {
		code, _, stderr := exec(t, nil, "--kubeconfig", filepath.Join(t.TempDir(), "missing"))
		if code != 1 || !strings.Contains(stderr, "kubeconfig file") || !strings.Contains(stderr, "cannot be read") {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
	})
	t.Run("unknown context", func(t *testing.T) {
		cfg := kubeconfig(t, "https://127.0.0.1:1")
		code, _, stderr := exec(t, nil, "--kubeconfig", cfg, "--context", "nope")
		if code != 1 || !strings.Contains(stderr, `context "nope" does not exist`) || !strings.Contains(stderr, "Available contexts: test") {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
	})
	t.Run("unreachable server", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		code, _, stderr := exec(t, nil, "--kubeconfig", kubeconfig(t, url))
		if code != 1 || !strings.Contains(stderr, "cannot reach API server "+url) {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
	})
	t.Run("unauthorized", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`))
		}))
		defer srv.Close()
		code, _, stderr := exec(t, nil, "--kubeconfig", kubeconfig(t, srv.URL))
		if code != 1 || !strings.Contains(stderr, srv.URL) || !strings.Contains(stderr, "rejected the credentials") || !strings.Contains(stderr, "provide credentials") {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
	})
	t.Run("bad flag", func(t *testing.T) {
		code, _, stderr := exec(t, nil, "--provider", "skynet")
		if code != 2 || !strings.Contains(stderr, "unknown provider") {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
	})
}

func kubeconfig(t *testing.T, server string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubeconfig")
	os.WriteFile(p, []byte(`apiVersion: v1
kind: Config
current-context: test
clusters:
- name: c
  cluster: {server: `+server+`}
users:
- name: u
  user: {token: t}
contexts:
- name: test
  context: {cluster: c, user: u}
`), 0o600)
	return p
}

func TestAuditCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := audit.Entry{Time: time.Now(), Intent: "scale web up", Tool: "scale", Args: json.RawMessage(`{"replicas":3}`),
		Target: audit.Target{Context: "c", Kind: "Deployment", Namespace: "shop", Name: "web"}}
	e.DryRun.Result, e.Decision.Action, e.Decision.By, e.Outcome.Status = audit.DryRunPassed, audit.DecisionApproved, "me", audit.OutcomeApplied
	l.Append(e)
	e.Decision.Action, e.Outcome.Status = audit.DecisionRejected, audit.OutcomeDeclined
	l.Append(e)
	l.Close()

	code, out, _ := exec(t, nil, "audit", "verify", "--audit-file", path)
	if code != 0 || !strings.Contains(out, "2 entries") || !strings.Contains(out, "OK") {
		t.Fatalf("verify: code %d\n%s", code, out)
	}
	code, out, _ = exec(t, nil, "audit", "show", "--audit-file", path)
	for _, want := range []string{"Integrity: OK", "#1", "scale", "Deployment shop/web", "you asked:  scale web up", "approved by me", "applied", "#2", "rejected by me", "declined"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Errorf("show: code %d", code)
	}

	// Tamper: flip the second decision.
	data, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(data, []byte(`"declined"`), []byte(`"applied"`), 1), 0o600)
	code, out, _ = exec(t, nil, "audit", "verify", "--audit-file", path)
	if code != 1 || !strings.Contains(out, "FAILED") || !strings.Contains(out, "line 2") {
		t.Fatalf("tampering not reported: code %d\n%s", code, out)
	}
}

func TestVersionAndConfig(t *testing.T) {
	code, out, _ := exec(t, nil, "version")
	if code != 0 || !strings.HasPrefix(out, "k8s-copilot ") {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = exec(t, map[string]string{"ANTHROPIC_API_KEY": "sk-secret"}, "config", "--model", "m1")
	if code != 0 || !strings.Contains(out, "ANTHROPIC_API_KEY (set)") || !strings.Contains(out, "m1") || strings.Contains(out, "sk-secret") {
		t.Fatalf("%d %q", code, out)
	}
}
