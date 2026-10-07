// Package tools is the complete list of things the agent can do to a cluster.
//
// The model never writes a command: it names one of these tools and passes
// structured arguments, which are decoded strictly into a typed Go struct.
// Tools come in two tiers:
//
//   - read tools run immediately and return text;
//   - mutate tools cannot run at all. They can only produce a Plan, and a Plan
//     can only be applied with an approval.Grant (see mutate.go).
//
// Anything not registered here does not exist as far as the agent is
// concerned. That, not the prompt, is what keeps delete/apply/image changes
// out of reach.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fardani235/k8s-copilot/internal/kube"
)

// Tier classifies a tool.
type Tier int

const (
	Read Tier = iota
	Mutate
)

func (t Tier) String() string {
	if t == Mutate {
		return "mutate"
	}
	return "read"
}

// Spec is what the model is told about a tool.
type Spec struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema of the arguments object
	Tier        Tier
}

// Options tune the registry.
type Options struct {
	// MaxResultBytes caps one read result handed to the model.
	MaxResultBytes int
	// MaxReplicas is the largest replica count `scale` will propose.
	MaxReplicas int
	// ProtectedNamespaces are namespaces in which no mutation is proposed.
	ProtectedNamespaces []string
	// RedactSecrets hides Secret values from read results (default on).
	RedactSecrets bool
}

// DefaultOptions are the shipped defaults.
func DefaultOptions() Options {
	return Options{MaxResultBytes: 24_000, MaxReplicas: 100, RedactSecrets: true}
}

type tool struct {
	Spec
	read func(ctx context.Context, raw json.RawMessage) (string, error)
	plan func(ctx context.Context, raw json.RawMessage) (*Plan, error)
}

// Registry maps tool names to typed operations on one cluster.
type Registry struct {
	c     *kube.Cluster
	opts  Options
	tools map[string]*tool
	order []string
}

// NewRegistry builds the v1 tool set: five read tools, four mutate tools.
func NewRegistry(c *kube.Cluster, opts Options) *Registry {
	if opts.MaxResultBytes <= 0 {
		opts.MaxResultBytes = DefaultOptions().MaxResultBytes
	}
	if opts.MaxReplicas <= 0 {
		opts.MaxReplicas = DefaultOptions().MaxReplicas
	}
	r := &Registry{c: c, opts: opts, tools: map[string]*tool{}}
	r.registerRead()
	r.registerMutate()
	return r
}

func (r *Registry) add(t *tool) {
	if (t.Tier == Read) != (t.read != nil) || (t.Tier == Mutate) != (t.plan != nil) {
		panic("tools: " + t.Name + " is wired to the wrong tier")
	}
	if !json.Valid(t.Schema) {
		panic("tools: " + t.Name + " has an invalid schema")
	}
	r.tools[t.Name] = t
	r.order = append(r.order, t.Name)
}

// Specs lists the tools in registration order.
func (r *Registry) Specs() []Spec {
	out := make([]Spec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n].Spec)
	}
	return out
}

// Lookup returns a tool's spec.
func (r *Registry) Lookup(name string) (Spec, bool) {
	t, ok := r.tools[name]
	if !ok {
		return Spec{}, false
	}
	return t.Spec, true
}

// UnknownToolError explains that a tool does not exist and, when the name
// looks like one of the deliberately unsupported operations, says so.
type UnknownToolError struct {
	Name      string
	Available []string
}

func (e *UnknownToolError) Error() string {
	msg := fmt.Sprintf("unknown tool %q; nothing was executed.", e.Name)
	if what := unsupportedOperation(e.Name); what != "" {
		msg = fmt.Sprintf("refused: %s is outside the set of changes k8s-copilot supports, and no tool for it exists; nothing was executed.", what)
	}
	return msg + " The only tools are: " + strings.Join(e.Available, ", ") +
		". k8s-copilot cannot delete resources, apply or patch arbitrary manifests, change container images, exec into containers, or cordon/drain nodes." +
		" If one of those is needed, explain it to the user so they can do it themselves."
}

// unsupportedOperation recognises attempts at out-of-scope operations by
// name, purely to give a clearer refusal. Enforcement does not depend on it:
// an unrecognised name is refused just the same.
func unsupportedOperation(name string) string {
	n := strings.ToLower(name)
	for _, c := range []struct{ frag, what string }{
		{"delete", "deleting resources"}, {"remove", "deleting resources"}, {"destroy", "deleting resources"},
		{"apply", "applying arbitrary manifests"}, {"create", "creating resources"}, {"replace", "replacing resources"},
		{"patch", "arbitrary patching"}, {"edit", "arbitrary editing"},
		{"image", "changing container images"},
		{"exec", "executing commands in containers"}, {"shell", "running shell commands"}, {"kubectl", "running kubectl commands"},
		{"run", "running commands"}, {"bash", "running shell commands"},
		{"drain", "draining nodes"}, {"cordon", "cordoning nodes"}, {"taint", "tainting nodes"},
		{"rollback", "rolling back workloads"}, {"undo", "rolling back workloads"},
	} {
		if strings.Contains(n, c.frag) {
			return c.what
		}
	}
	return ""
}

func (r *Registry) get(name string, want Tier) (*tool, error) {
	t, ok := r.tools[name]
	if !ok {
		avail := append([]string(nil), r.order...)
		sort.Strings(avail)
		return nil, &UnknownToolError{Name: name, Available: avail}
	}
	if t.Tier != want {
		return nil, fmt.Errorf("tool %q is a %s tool and cannot be used as a %s tool", name, t.Tier, want)
	}
	return t, nil
}

// Read executes a read tool. It refuses mutate tools.
func (r *Registry) Read(ctx context.Context, name string, args json.RawMessage) (string, error) {
	t, err := r.get(name, Read)
	if err != nil {
		return "", err
	}
	return t.read(ctx, args)
}

// Plan validates a mutate tool call and prepares — without sending — the
// change. It refuses read tools. The returned Plan still has to pass DryRun
// and be approved before Apply will do anything.
func (r *Registry) Plan(ctx context.Context, name string, args json.RawMessage) (*Plan, error) {
	t, err := r.get(name, Mutate)
	if err != nil {
		return nil, err
	}
	return t.plan(ctx, args)
}

// ArgError is a tool call whose arguments do not match the tool's schema.
type ArgError struct {
	Tool string
	Msg  string
}

func (e *ArgError) Error() string {
	return fmt.Sprintf("invalid arguments for %s: %s; nothing was executed.", e.Tool, e.Msg)
}

// decode strictly unmarshals raw into dst: unknown fields, wrong types and
// trailing data are all errors.
func decode(toolName string, raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &ArgError{Tool: toolName, Msg: strings.TrimPrefix(err.Error(), "json: ")}
	}
	if dec.More() {
		return &ArgError{Tool: toolName, Msg: "unexpected data after the arguments object"}
	}
	return nil
}

func require(toolName string, fields ...string) error {
	for i := 0; i+1 < len(fields); i += 2 {
		if strings.TrimSpace(fields[i+1]) == "" {
			return &ArgError{Tool: toolName, Msg: fmt.Sprintf("%q is required", fields[i])}
		}
	}
	return nil
}

// schema builds a JSON Schema object. props maps name → property schema
// (already JSON); required lists required names.
func schema(props map[string]string, required ...string) json.RawMessage {
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%s", n, props[n])
	}
	b.WriteString(`},"required":[`)
	for i, n := range required {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q", n)
	}
	b.WriteString(`],"additionalProperties":false}`)
	return json.RawMessage(b.String())
}

func strProp(desc string) string {
	return fmt.Sprintf(`{"type":"string","description":%q}`, desc)
}
func intProp(desc string) string {
	return fmt.Sprintf(`{"type":"integer","description":%q}`, desc)
}
func boolProp(desc string) string {
	return fmt.Sprintf(`{"type":"boolean","description":%q}`, desc)
}
