package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func parse(t *testing.T, e map[string]string, args ...string) Config {
	t.Helper()
	// Keep the developer's real config file out of the test.
	if e == nil {
		e = map[string]string{}
	}
	if _, ok := e["K2STUI_CONFIG"]; !ok && !contains(args, "--config") {
		args = append(args, "--config", emptyConfig(t))
	}
	inv, err := Parse(args, env(e), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return inv.Config
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func emptyConfig(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "empty.yaml")
	os.WriteFile(p, []byte("{}\n"), 0o600)
	return p
}

// 8.1: defaults.
func TestDefaults(t *testing.T) {
	c := parse(t, nil)
	if c.Provider != "anthropic" || c.KeyEnv() != "ANTHROPIC_API_KEY" || c.RefreshInterval != 5*time.Second ||
		c.MaxIterations != 12 || c.MaxToolCalls != 30 || c.Context != "" || c.Kubeconfig != "" ||
		!c.RedactSecrets || c.MaxReplicas != 100 || !strings.HasSuffix(c.AuditFile, filepath.Join("k2stui", "audit.jsonl")) {
		t.Fatalf("defaults: %+v", c)
	}
}

// 8.1: file < environment < flags.
func TestPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(`
provider: openrouter
model: from-file
api_key_env: MY_KEY
refresh_interval: 30s
max_iterations: 5
max_tool_calls: 9
audit_file: /tmp/from-file.jsonl
context: file-ctx
protected_namespaces: [kube-system, prod]
redact_secrets: true
request_timeout: 45s
`), 0o600)

	c := parse(t, nil, "--config", path)
	if c.Provider != "openrouter" || c.Model != "from-file" || c.KeyEnv() != "MY_KEY" || c.RefreshInterval != 30*time.Second ||
		c.MaxIterations != 5 || c.MaxToolCalls != 9 || c.AuditFile != "/tmp/from-file.jsonl" || c.Context != "file-ctx" ||
		strings.Join(c.ProtectedNamespaces, ",") != "kube-system,prod" || c.RequestTimeout != 45*time.Second || c.Source != path {
		t.Fatalf("from file: %+v", c)
	}

	e := map[string]string{"K2STUI_CONFIG": path, "K2STUI_MODEL": "from-env", "K2STUI_MAX_ITERATIONS": "7", "K2STUI_CONTEXT": "env-ctx", "K2STUI_REFRESH_INTERVAL": "off"}
	c = parse(t, e)
	if c.Model != "from-env" || c.MaxIterations != 7 || c.Context != "env-ctx" || c.RefreshInterval != 0 || c.Provider != "openrouter" {
		t.Fatalf("env over file: %+v", c)
	}

	c = parse(t, e, "--model", "from-flag", "--context", "flag-ctx", "--max-tool-calls", "3", "--refresh", "2s", "--audit-file", "/tmp/flag.jsonl", "--provider", "anthropic", "-n", "shop")
	if c.Model != "from-flag" || c.Context != "flag-ctx" || c.MaxToolCalls != 3 || c.RefreshInterval != 2*time.Second ||
		c.AuditFile != "/tmp/flag.jsonl" || c.Provider != "anthropic" || c.Namespace != "shop" || c.MaxIterations != 7 {
		t.Fatalf("flags over env: %+v", c)
	}
}

func TestRejectsBadConfiguration(t *testing.T) {
	bad := func(name, wantErr string, e map[string]string, args ...string) {
		t.Helper()
		if e == nil {
			e = map[string]string{}
		}
		if !contains(args, "--config") {
			args = append(args, "--config", emptyConfig(t))
		}
		_, err := Parse(args, env(e), io.Discard)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: got %v, want %q", name, err, wantErr)
		}
	}
	bad("unknown provider", "unknown provider", nil, "--provider", "skynet")
	bad("zero iterations", "max_iterations", nil, "--max-iterations", "0")
	bad("refresh too fast", "refresh_interval", nil, "--refresh", "10ms")
	bad("bad env number", "not a number", map[string]string{"K2STUI_MAX_TOOL_CALLS": "lots"})
	bad("stray argument", "unexpected argument", nil, "pods")
	bad("missing explicit config", "cannot read config file", nil, "--config", "/nonexistent/k2stui.yaml")

	// A typo in the file is an error, not a silently ignored setting — and a
	// key in the file gets a pointed message.
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("api_key: sk-oops\n"), 0o600)
	_, err := Parse([]string{"--config", path}, env(nil), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "API keys are not stored in the config file") {
		t.Errorf("api_key in file: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "sk-oops") {
		t.Errorf("the error repeats the key: %v", err)
	}
}

// 4.5: a missing credential is a clear, actionable error naming the variable.
func TestProviderCredential(t *testing.T) {
	c := parse(t, nil)
	_, err := c.ProviderConfig(env(nil))
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY is not set") || !strings.Contains(err.Error(), "export ANTHROPIC_API_KEY") {
		t.Fatalf("got %v", err)
	}
	pc, err := c.ProviderConfig(env(map[string]string{"ANTHROPIC_API_KEY": "sk-1"}))
	if err != nil || pc.APIKey != "sk-1" || pc.Provider != "anthropic" {
		t.Fatalf("%+v %v", pc, err)
	}

	c = parse(t, nil, "--provider", "openai", "--api-key-env", "WORK_OPENAI_KEY", "--model", "m")
	if _, err := c.ProviderConfig(env(map[string]string{"OPENAI_API_KEY": "wrong-var"})); err == nil || !strings.Contains(err.Error(), "WORK_OPENAI_KEY") {
		t.Fatalf("custom key variable: %v", err)
	}

	// Describe never prints the key.
	out := c.Describe(env(map[string]string{"WORK_OPENAI_KEY": "sk-very-secret"}))
	if strings.Contains(out, "sk-very-secret") || !strings.Contains(out, "WORK_OPENAI_KEY (set)") {
		t.Fatalf("describe:\n%s", out)
	}
}

func TestSubcommands(t *testing.T) {
	for args, want := range map[string]string{
		"audit verify": "audit-verify", "audit show": "audit-show", "config": "config", "version": "version", "--version": "version", "": "",
	} {
		a := strings.Fields(args)
		a = append(a, "--config", emptyConfig(t))
		inv, err := Parse(a, env(nil), io.Discard)
		if err != nil || inv.Command != want {
			t.Errorf("%q: command %q, err %v", args, inv.Command, err)
		}
	}
}
