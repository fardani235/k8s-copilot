package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// ResourceType is one listable resource type served by the cluster.
type ResourceType struct {
	Group      string
	Version    string
	Resource   string // plural, e.g. "deployments"
	Kind       string
	Singular   string
	ShortNames []string
	Namespaced bool
	Verbs      []string
}

func (t ResourceType) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: t.Group, Version: t.Version, Resource: t.Resource}
}

// APIVersion is "v1" or "group/version".
func (t ResourceType) APIVersion() string {
	return schema.GroupVersion{Group: t.Group, Version: t.Version}.String()
}

// String is the kubectl-style qualified name: "pods", "deployments.apps".
func (t ResourceType) String() string {
	if t.Group == "" {
		return t.Resource
	}
	return t.Resource + "." + t.Group
}

func (t ResourceType) IsZero() bool { return t.Resource == "" }

func (t ResourceType) can(verb string) bool {
	for _, v := range t.Verbs {
		if v == verb {
			return true
		}
	}
	return false
}

// DiscoveryWarning means discovery succeeded only partly: some API groups
// could not be queried (typically a stale aggregated API such as a broken
// metrics-server). The types that were returned are real; the listed groups
// are missing and the UI says so.
type DiscoveryWarning struct{ Groups []string }

func (w *DiscoveryWarning) Error() string {
	return "discovery incomplete, these API groups did not answer: " + strings.Join(w.Groups, ", ")
}

type typeCache struct {
	mu    sync.Mutex
	types []ResourceType
	warn  *DiscoveryWarning
	ok    bool
}

// Types returns every listable resource type the cluster serves, in its
// preferred version. A nil error with a non-nil warning means a partial
// result; an error means discovery failed and nothing should be shown.
func (c *Cluster) Types(ctx context.Context, refresh bool) ([]ResourceType, *DiscoveryWarning, error) {
	c.typeCache.mu.Lock()
	defer c.typeCache.mu.Unlock()
	if c.typeCache.ok && !refresh {
		return c.typeCache.types, c.typeCache.warn, nil
	}
	if inv, ok := c.disco.(discovery.CachedDiscoveryInterface); ok && refresh {
		inv.Invalidate()
	}

	lists, err := c.disco.ServerPreferredResources()
	var warn *DiscoveryWarning
	if err != nil {
		var gf *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &gf) || len(lists) == 0 {
			return nil, nil, fmt.Errorf("cannot discover the cluster's resource types: %w", err)
		}
		warn = &DiscoveryWarning{}
		for gv := range gf.Groups {
			warn.Groups = append(warn.Groups, gv.String())
		}
		sort.Strings(warn.Groups)
	}

	types := typesFromLists(lists)
	if len(types) == 0 {
		return nil, nil, errors.New("cannot discover the cluster's resource types: the server returned no listable resources")
	}
	c.typeCache.types, c.typeCache.warn, c.typeCache.ok = types, warn, true
	return types, warn, nil
}

func typesFromLists(lists []*metav1.APIResourceList) []ResourceType {
	var types []ResourceType
	seen := map[schema.GroupResource]bool{}
	for _, l := range lists {
		if l == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") { // subresource
				continue
			}
			t := ResourceType{
				Group: gv.Group, Version: gv.Version, Resource: r.Name, Kind: r.Kind,
				Singular: r.SingularName, ShortNames: r.ShortNames, Namespaced: r.Namespaced, Verbs: r.Verbs,
			}
			if !t.can("list") {
				continue
			}
			gr := schema.GroupResource{Group: t.Group, Resource: t.Resource}
			if seen[gr] {
				continue
			}
			seen[gr] = true
			types = append(types, t)
		}
	}
	sort.SliceStable(types, func(i, j int) bool {
		if types[i].Resource != types[j].Resource {
			return types[i].Resource < types[j].Resource
		}
		return groupRank(types[i].Group) < groupRank(types[j].Group)
	})
	return types
}

// groupRank orders same-named resources so the built-in one wins, the way
// kubectl resolves "events" or "ingresses".
func groupRank(g string) string {
	switch {
	case g == "":
		return "0"
	case g == "apps" || g == "batch":
		return "1" + g
	case strings.HasSuffix(g, ".k8s.io"):
		return "2" + g
	default:
		return "3" + g
	}
}

// Resolve finds a type by what a human or a model would call it: plural,
// singular, kind, short name, or "resource.group". Matching is
// case-insensitive.
func (c *Cluster) Resolve(ctx context.Context, name string) (ResourceType, error) {
	types, _, err := c.Types(ctx, false)
	if err != nil {
		return ResourceType{}, err
	}
	return ResolveIn(types, name)
}

// ResolveIn is Resolve over an explicit type list.
func ResolveIn(types []ResourceType, name string) (ResourceType, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return ResourceType{}, errors.New("resource type is empty")
	}
	res, group, qualified := strings.Cut(want, ".")

	var best *ResourceType
	consider := func(t *ResourceType) {
		if best == nil || groupRank(t.Group) < groupRank(best.Group) {
			best = t
		}
	}
	for i := range types {
		t := &types[i]
		if qualified && strings.EqualFold(t.Group, group) && t.matches(res) {
			return *t, nil
		}
		if t.matches(want) {
			consider(t)
		}
	}
	if best != nil {
		return *best, nil
	}

	var near []string
	for _, t := range types {
		if strings.Contains(t.Resource, res) || strings.Contains(strings.ToLower(t.Kind), res) {
			near = append(near, t.String())
		}
		if len(near) == 8 {
			break
		}
	}
	msg := fmt.Sprintf("the cluster does not serve a resource type called %q", name)
	if len(near) > 0 {
		msg += " (did you mean: " + strings.Join(near, ", ") + "?)"
	}
	return ResourceType{}, errors.New(msg)
}

func (t *ResourceType) matches(s string) bool {
	if t.Resource == s || strings.EqualFold(t.Kind, s) || (t.Singular != "" && t.Singular == s) {
		return true
	}
	for _, sn := range t.ShortNames {
		if sn == s {
			return true
		}
	}
	return false
}
