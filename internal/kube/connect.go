// Package kube is the only package that talks to the Kubernetes API.
//
// Everything in here is read-only except write.go, which holds the single
// write primitive used by the gated mutate tools. That boundary is enforced by
// internal/guard's source-level tests.
package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	// Auth plugins kubectl supports (OIDC, exec credential helpers such as
	// aws/gke/azure), so any kubeconfig that works with kubectl works here.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// ConnectOptions selects how the kubeconfig is resolved. The zero value means
// "whatever kubectl would use": $KUBECONFIG, else ~/.kube/config, current
// context.
type ConnectOptions struct {
	Kubeconfig string // explicit path; empty = standard rules
	Context    string // explicit context; empty = current-context
	UserAgent  string
}

// Info describes the resolved connection. It is shown in the header, injected
// into the copilot's context, and written to every audit entry.
type Info struct {
	Context   string
	Cluster   string
	Server    string
	User      string
	Namespace string // the context's default namespace ("default" if unset)
}

// ConfigError is a kubeconfig problem the user has to fix before anything can
// start.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

// ConnectError is a failure to talk to the API server named in Server.
type ConnectError struct {
	Server       string
	Unauthorized bool
	Err          error
}

func (e *ConnectError) Error() string {
	if e.Unauthorized {
		return fmt.Sprintf("API server %s rejected the credentials of the selected kubeconfig context: %v\n"+
			"Check that the context's user is still valid (expired token or certificate?) — `kubectl auth whoami` shows what the server sees.", e.Server, e.Err)
	}
	return fmt.Sprintf("cannot reach API server %s: %v\n"+
		"Check that the cluster is running and reachable from here (VPN, port-forward, `minikube status`, …).", e.Server, e.Err)
}

func (e *ConnectError) Unwrap() error { return e.Err }

// LoadConfig resolves the kubeconfig by the standard rules and returns a REST
// config for the chosen context.
func LoadConfig(opts ConnectOptions) (*rest.Config, Info, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		rules.ExplicitPath = opts.Kubeconfig
	} else if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) == "" {
		// Same file kubectl falls back to, but resolved now rather than when
		// client-go was initialised.
		rules.Precedence = []string{filepath.Join(homedir.HomeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)}
	}
	searched := strings.Join(rules.GetLoadingPrecedence(), ", ")
	if opts.Kubeconfig != "" {
		searched = opts.Kubeconfig
	}

	if opts.Kubeconfig != "" {
		if _, err := os.Stat(opts.Kubeconfig); err != nil {
			return nil, Info{}, &ConfigError{Msg: fmt.Sprintf("kubeconfig file %s cannot be read: %v", opts.Kubeconfig, errors.Unwrap(err))}
		}
	}
	raw, err := rules.Load()
	if err != nil {
		return nil, Info{}, &ConfigError{Msg: fmt.Sprintf("cannot load kubeconfig (%s): %v", searched, err)}
	}
	if len(raw.Contexts) == 0 {
		return nil, Info{}, &ConfigError{Msg: fmt.Sprintf(
			"no kubeconfig with a usable context was found (looked at: %s).\n"+
				"k2stui connects the same way kubectl does: set KUBECONFIG or create ~/.kube/config.", searched)}
	}

	name := raw.CurrentContext
	if opts.Context != "" {
		name = opts.Context
	}
	if name == "" {
		return nil, Info{}, &ConfigError{Msg: fmt.Sprintf(
			"the kubeconfig has no current-context. Pick one with `kubectl config use-context <name>` or pass --context.\nAvailable contexts: %s",
			contextNames(raw.Contexts))}
	}
	kctx, ok := raw.Contexts[name]
	if !ok {
		return nil, Info{}, &ConfigError{Msg: fmt.Sprintf(
			"context %q does not exist in the kubeconfig.\nAvailable contexts: %s", name, contextNames(raw.Contexts))}
	}

	cc := clientcmd.NewNonInteractiveClientConfig(*raw, name, &clientcmd.ConfigOverrides{}, rules)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, Info{}, &ConfigError{Msg: fmt.Sprintf("context %q is not usable: %v", name, err)}
	}
	cfg.QPS, cfg.Burst = 50, 100
	if opts.UserAgent != "" {
		cfg.UserAgent = opts.UserAgent
	}

	ns := kctx.Namespace
	if ns == "" {
		ns = "default"
	}
	return cfg, Info{
		Context:   name,
		Cluster:   kctx.Cluster,
		Server:    cfg.Host,
		User:      kctx.AuthInfo,
		Namespace: ns,
	}, nil
}

func contextNames[T any](m map[string]T) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Cluster is a connection to one cluster.
type Cluster struct {
	Info Info

	dyn   dynamic.Interface
	cs    kubernetes.Interface
	disco discovery.DiscoveryInterface

	// Raw HTTP access for server-side Table printing. Nil when built from
	// fakes; listing then falls back to generic columns.
	http    *http.Client
	baseURL *url.URL

	typeCache typeCache
}

// Connect resolves the kubeconfig, builds the clients, and probes the server
// so that an unreachable or unauthorised cluster is reported up front instead
// of looking like an empty one.
func Connect(ctx context.Context, opts ConnectOptions) (*Cluster, error) {
	cfg, info, err := LoadConfig(opts)
	if err != nil {
		return nil, err
	}
	c, err := NewForConfig(cfg, info)
	if err != nil {
		return nil, err
	}
	if err := c.Probe(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// NewForConfig builds the clients without contacting the server.
func NewForConfig(cfg *rest.Config, info Info) (*Cluster, error) {
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("context %q is not usable: %v", info.Context, err)}
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, err
	}
	base, _, err := rest.DefaultServerURL(cfg.Host, "", schema.GroupVersion{}, true)
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("context %q has an invalid server address %q: %v", info.Context, cfg.Host, err)}
	}
	return &Cluster{Info: info, dyn: dyn, cs: cs, disco: cs.Discovery(), http: hc, baseURL: base}, nil
}

// NewForClients builds a Cluster from existing clients (client-go fakes in
// tests).
func NewForClients(info Info, dyn dynamic.Interface, cs kubernetes.Interface, disco discovery.DiscoveryInterface) *Cluster {
	return &Cluster{Info: info, dyn: dyn, cs: cs, disco: disco}
}

// Probe makes one cheap authenticated request and classifies the failure.
func (c *Cluster) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// /version is readable by any authenticated user under default RBAC; an
	// invalid credential gets 401 and a dead server a transport error.
	_, err := c.cs.Discovery().RESTClient().Get().AbsPath("/version").DoRaw(ctx)
	if err == nil {
		return nil
	}
	ce := &ConnectError{Server: c.Info.Server, Err: err}
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
		ce.Unauthorized = true
	}
	return ce
}

// IsDenied reports whether err is the cluster refusing the caller (401/403),
// as opposed to the thing not existing.
func IsDenied(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err)
}

// Reason returns a short human explanation of an API error.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	var st apierrors.APIStatus
	if errors.As(err, &st) {
		s := st.Status()
		if s.Message != "" {
			return s.Message
		}
	}
	return err.Error()
}
