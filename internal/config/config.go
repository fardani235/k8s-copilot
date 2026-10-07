// Package config resolves k8s-copilot's settings.
//
// Precedence, lowest to highest: built-in defaults, the config file,
// K8S_COPILOT_* environment variables, command-line flags. Nothing is required:
// with no file, no variables and no flags, k8s-copilot connects to the current
// kubeconfig context and uses Anthropic with ANTHROPIC_API_KEY.
//
// API keys are never configuration. The config can only name the environment
// variable that holds one.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/fardani235/k8s-copilot/internal/audit"
	"github.com/fardani235/k8s-copilot/internal/llm"
)

// Config is the effective configuration.
type Config struct {
	// Cluster
	Kubeconfig string // empty = standard rules
	Context    string // empty = current-context
	Namespace  string // initial namespace; empty = the context's default

	// Model provider
	Provider       string
	Model          string // empty = provider default
	BaseURL        string // empty = provider default
	APIKeyEnv      string // empty = provider default variable
	MaxTokens      int
	RequestTimeout time.Duration

	// Agent bounds
	MaxIterations int
	MaxToolCalls  int

	// Safety
	AuditFile           string
	MaxReplicas         int
	ProtectedNamespaces []string
	RedactSecrets       bool
	MaxResultBytes      int

	// Browser
	RefreshInterval time.Duration // 0 disables periodic refresh

	// Source is the config file that was read, if any.
	Source string
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Provider:        "anthropic",
		MaxTokens:       4096,
		RequestTimeout:  120 * time.Second,
		MaxIterations:   12,
		MaxToolCalls:    30,
		AuditFile:       audit.DefaultPath(),
		MaxReplicas:     100,
		RedactSecrets:   true,
		MaxResultBytes:  24_000,
		RefreshInterval: 5 * time.Second,
	}
}

// file is the on-disk shape. Pointers distinguish "absent" from zero.
type file struct {
	Kubeconfig          *string   `json:"kubeconfig"`
	Context             *string   `json:"context"`
	Namespace           *string   `json:"namespace"`
	Provider            *string   `json:"provider"`
	Model               *string   `json:"model"`
	BaseURL             *string   `json:"base_url"`
	APIKeyEnv           *string   `json:"api_key_env"`
	MaxTokens           *int      `json:"max_tokens"`
	RequestTimeout      *string   `json:"request_timeout"`
	MaxIterations       *int      `json:"max_iterations"`
	MaxToolCalls        *int      `json:"max_tool_calls"`
	AuditFile           *string   `json:"audit_file"`
	MaxReplicas         *int      `json:"max_replicas"`
	ProtectedNamespaces *[]string `json:"protected_namespaces"`
	RedactSecrets       *bool     `json:"redact_secrets"`
	MaxResultBytes      *int      `json:"max_result_bytes"`
	RefreshInterval     *string   `json:"refresh_interval"`
}

// DefaultFile is $XDG_CONFIG_HOME/k8s-copilot/config.yaml (~/.config/… by default).
func DefaultFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "k8s-copilot", "config.yaml")
}

func (c *Config) applyFile(path string, mustExist bool) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !mustExist {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot read config file: %w", err)
	}
	var f file
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		hint := ""
		if strings.Contains(err.Error(), "api_key\"") || strings.Contains(err.Error(), "api_key ") {
			hint = " (API keys are not stored in the config file: put the key in an environment variable and name that variable with api_key_env)"
		}
		return fmt.Errorf("config file %s: %v%s", path, err, hint)
	}
	c.Source = path
	setStr := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	setInt := func(dst *int, src *int) {
		if src != nil {
			*dst = *src
		}
	}
	setStr(&c.Kubeconfig, f.Kubeconfig)
	setStr(&c.Context, f.Context)
	setStr(&c.Namespace, f.Namespace)
	setStr(&c.Provider, f.Provider)
	setStr(&c.Model, f.Model)
	setStr(&c.BaseURL, f.BaseURL)
	setStr(&c.APIKeyEnv, f.APIKeyEnv)
	setStr(&c.AuditFile, f.AuditFile)
	setInt(&c.MaxTokens, f.MaxTokens)
	setInt(&c.MaxIterations, f.MaxIterations)
	setInt(&c.MaxToolCalls, f.MaxToolCalls)
	setInt(&c.MaxReplicas, f.MaxReplicas)
	setInt(&c.MaxResultBytes, f.MaxResultBytes)
	if f.ProtectedNamespaces != nil {
		c.ProtectedNamespaces = *f.ProtectedNamespaces
	}
	if f.RedactSecrets != nil {
		c.RedactSecrets = *f.RedactSecrets
	}
	if f.RequestTimeout != nil {
		if c.RequestTimeout, err = time.ParseDuration(*f.RequestTimeout); err != nil {
			return fmt.Errorf("config file %s: request_timeout: %v", path, err)
		}
	}
	if f.RefreshInterval != nil {
		if c.RefreshInterval, err = parseRefresh(*f.RefreshInterval); err != nil {
			return fmt.Errorf("config file %s: refresh_interval: %v", path, err)
		}
	}
	return nil
}

func parseRefresh(s string) (time.Duration, error) {
	if s == "0" || s == "off" || s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

func (c *Config) applyEnv(getenv func(string) string) error {
	str := func(name string, dst *string) {
		if v := getenv(name); v != "" {
			*dst = v
		}
	}
	num := func(name string, dst *int) error {
		v := getenv(name)
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s: %q is not a number", name, v)
		}
		*dst = n
		return nil
	}
	str("K8S_COPILOT_KUBECONFIG", &c.Kubeconfig)
	str("K8S_COPILOT_CONTEXT", &c.Context)
	str("K8S_COPILOT_NAMESPACE", &c.Namespace)
	str("K8S_COPILOT_PROVIDER", &c.Provider)
	str("K8S_COPILOT_MODEL", &c.Model)
	str("K8S_COPILOT_BASE_URL", &c.BaseURL)
	str("K8S_COPILOT_API_KEY_ENV", &c.APIKeyEnv)
	str("K8S_COPILOT_AUDIT_FILE", &c.AuditFile)
	if err := num("K8S_COPILOT_MAX_ITERATIONS", &c.MaxIterations); err != nil {
		return err
	}
	if err := num("K8S_COPILOT_MAX_TOOL_CALLS", &c.MaxToolCalls); err != nil {
		return err
	}
	if v := getenv("K8S_COPILOT_REFRESH_INTERVAL"); v != "" {
		d, err := parseRefresh(v)
		if err != nil {
			return fmt.Errorf("K8S_COPILOT_REFRESH_INTERVAL: %v", err)
		}
		c.RefreshInterval = d
	}
	return nil
}

// Validate checks ranges. It does not check the provider credential; see
// ProviderConfig.
func (c Config) Validate() error {
	switch {
	case c.MaxIterations < 1 || c.MaxIterations > 100:
		return errors.New("max_iterations must be between 1 and 100")
	case c.MaxToolCalls < 1 || c.MaxToolCalls > 500:
		return errors.New("max_tool_calls must be between 1 and 500")
	case c.MaxReplicas < 1:
		return errors.New("max_replicas must be at least 1")
	case c.RefreshInterval != 0 && c.RefreshInterval < time.Second:
		return errors.New("refresh_interval must be at least 1s (or 0/off to disable)")
	case c.AuditFile == "":
		return errors.New("audit_file must not be empty")
	}
	if _, ok := llm.Presets[c.Provider]; !ok {
		return fmt.Errorf("unknown provider %q (choose one of: %s)", c.Provider, strings.Join(llm.ProviderNames(), ", "))
	}
	return nil
}

// KeyEnv is the environment variable the API key is read from.
func (c Config) KeyEnv() string {
	if c.APIKeyEnv != "" {
		return c.APIKeyEnv
	}
	return llm.Presets[c.Provider].KeyEnv
}

// ProviderConfig resolves the provider settings, reading the API key from the
// environment. A missing key is reported with the exact variable to set.
func (c Config) ProviderConfig(getenv func(string) string) (llm.Config, error) {
	preset, ok := llm.Presets[c.Provider]
	if !ok {
		return llm.Config{}, fmt.Errorf("unknown provider %q (choose one of: %s)", c.Provider, strings.Join(llm.ProviderNames(), ", "))
	}
	key := getenv(c.KeyEnv())
	if key == "" && !preset.KeyOptional {
		return llm.Config{}, fmt.Errorf(
			"no API key for provider %q: the environment variable %s is not set.\nSet it (export %s=…) and restart, or choose another provider with --provider (%s).",
			c.Provider, c.KeyEnv(), c.KeyEnv(), strings.Join(llm.ProviderNames(), ", "))
	}
	return llm.Config{
		Provider: c.Provider, Model: c.Model, BaseURL: c.BaseURL, APIKey: key,
		MaxTokens: c.MaxTokens, Timeout: c.RequestTimeout,
	}, nil
}

// Invocation is a parsed command line.
type Invocation struct {
	Command string // "", "audit-verify", "audit-show", "config", "version"
	Config  Config
}

// ErrHelp is returned when usage was requested; the text has been written.
var ErrHelp = flag.ErrHelp

const usage = `k8s-copilot — a Kubernetes terminal browser with an AI copilot that asks before it acts.

Usage:
  k8s-copilot [flags]                 start the browser and copilot
  k8s-copilot audit verify [flags]    check the audit trail's integrity
  k8s-copilot audit show [flags]      print the audit trail
  k8s-copilot config [flags]          print the effective configuration
  k8s-copilot version

Flags:
`

// Parse resolves the configuration from args (without the program name) and
// the environment.
func Parse(args []string, getenv func(string) string, stderr io.Writer) (Invocation, error) {
	inv := Invocation{}
	switch {
	case len(args) >= 2 && args[0] == "audit" && (args[1] == "verify" || args[1] == "show"):
		inv.Command, args = "audit-"+args[1], args[2:]
	case len(args) >= 1 && args[0] == "audit":
		return inv, errors.New("usage: k8s-copilot audit verify | k8s-copilot audit show")
	case len(args) >= 1 && (args[0] == "config" || args[0] == "version"):
		inv.Command, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("k8s-copilot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nConfig file: %s (optional). Environment: K8S_COPILOT_PROVIDER, K8S_COPILOT_MODEL, K8S_COPILOT_BASE_URL,\nK8S_COPILOT_API_KEY_ENV, K8S_COPILOT_CONTEXT, K8S_COPILOT_NAMESPACE, K8S_COPILOT_AUDIT_FILE, K8S_COPILOT_REFRESH_INTERVAL,\nK8S_COPILOT_MAX_ITERATIONS, K8S_COPILOT_MAX_TOOL_CALLS. See docs/configuration.md.\n", DefaultFile())
	}
	var (
		cfgFile    = fs.String("config", "", "config file (default: "+DefaultFile()+")")
		kubeconfig = fs.String("kubeconfig", "", "kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
		kctx       = fs.String("context", "", "kubeconfig context to use (default: the current context)")
		namespace  = fs.String("namespace", "", "namespace to start in (default: the context's namespace)")
		provider   = fs.String("provider", "", "model provider: "+strings.Join(llm.ProviderNames(), ", ")+" (default: anthropic)")
		model      = fs.String("model", "", "model name (default: the provider's default)")
		baseURL    = fs.String("base-url", "", "provider API base URL (required for openai-compatible)")
		keyEnv     = fs.String("api-key-env", "", "environment variable holding the provider API key")
		refresh    = fs.String("refresh", "", "listing refresh interval, e.g. 5s; 0 or off disables (default: 5s)")
		maxIter    = fs.Int("max-iterations", 0, "model turns allowed per request (default: 12)")
		maxCalls   = fs.Int("max-tool-calls", 0, "tool calls allowed per request (default: 30)")
		auditFile  = fs.String("audit-file", "", "audit trail file (default: "+audit.DefaultPath()+")")
		version    = fs.Bool("version", false, "print the version and exit")
	)
	fs.StringVar(namespace, "n", "", "shorthand for --namespace")
	if err := fs.Parse(args); err != nil {
		return inv, err
	}
	if fs.NArg() > 0 {
		return inv, fmt.Errorf("unexpected argument %q (see k8s-copilot --help)", fs.Arg(0))
	}
	if *version {
		inv.Command = "version"
	}

	cfg := Default()
	path, explicit := *cfgFile, *cfgFile != ""
	if !explicit {
		if v := getenv("K8S_COPILOT_CONFIG"); v != "" {
			path, explicit = v, true
		} else {
			path = DefaultFile()
		}
	}
	if path != "" {
		if err := cfg.applyFile(path, explicit); err != nil {
			return inv, err
		}
	}
	if err := cfg.applyEnv(getenv); err != nil {
		return inv, err
	}

	var ferr error
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "kubeconfig":
			cfg.Kubeconfig = *kubeconfig
		case "context":
			cfg.Context = *kctx
		case "namespace", "n":
			cfg.Namespace = *namespace
		case "provider":
			cfg.Provider = *provider
		case "model":
			cfg.Model = *model
		case "base-url":
			cfg.BaseURL = *baseURL
		case "api-key-env":
			cfg.APIKeyEnv = *keyEnv
		case "max-iterations":
			cfg.MaxIterations = *maxIter
		case "max-tool-calls":
			cfg.MaxToolCalls = *maxCalls
		case "audit-file":
			cfg.AuditFile = *auditFile
		case "refresh":
			d, err := parseRefresh(*refresh)
			if err != nil {
				ferr = fmt.Errorf("--refresh: %v", err)
			}
			cfg.RefreshInterval = d
		}
	})
	if ferr != nil {
		return inv, ferr
	}
	if err := cfg.Validate(); err != nil {
		return inv, fmt.Errorf("invalid configuration: %w", err)
	}
	inv.Config = cfg
	return inv, nil
}

// Describe prints the effective configuration. It shows which variable the
// key comes from and whether it is set, never the key.
func (c Config) Describe(getenv func(string) string) string {
	or := func(s, d string) string {
		if s == "" {
			return d
		}
		return s
	}
	preset := llm.Presets[c.Provider]
	keyState := "not set"
	if getenv(c.KeyEnv()) != "" {
		keyState = "set"
	} else if preset.KeyOptional {
		keyState = "not set (optional for this provider)"
	}
	refresh := "off"
	if c.RefreshInterval > 0 {
		refresh = c.RefreshInterval.String()
	}
	var b strings.Builder
	w := func(k, v string) { fmt.Fprintf(&b, "%-22s %s\n", k+":", v) }
	w("config file", or(c.Source, "(none)"))
	w("kubeconfig", or(c.Kubeconfig, "(standard: $KUBECONFIG, then ~/.kube/config)"))
	w("context", or(c.Context, "(current-context)"))
	w("namespace", or(c.Namespace, "(the context's namespace)"))
	w("provider", c.Provider)
	w("model", or(c.Model, or(preset.Model, "(none: set --model)")))
	w("base_url", or(c.BaseURL, or(preset.BaseURL, "(none: set --base-url)")))
	w("api_key_env", c.KeyEnv()+" ("+keyState+")")
	w("max_tokens", strconv.Itoa(c.MaxTokens))
	w("request_timeout", c.RequestTimeout.String())
	w("max_iterations", strconv.Itoa(c.MaxIterations))
	w("max_tool_calls", strconv.Itoa(c.MaxToolCalls))
	w("audit_file", c.AuditFile)
	w("max_replicas", strconv.Itoa(c.MaxReplicas))
	w("protected_namespaces", or(strings.Join(c.ProtectedNamespaces, ", "), "(none)"))
	w("redact_secrets", strconv.FormatBool(c.RedactSecrets))
	w("max_result_bytes", strconv.Itoa(c.MaxResultBytes))
	w("refresh_interval", refresh)
	return b.String()
}
