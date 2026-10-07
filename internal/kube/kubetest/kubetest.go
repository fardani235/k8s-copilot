// Package kubetest builds a kube.Cluster on client-go fakes so that the
// browser, the tools, the agent loop and the approval flow can be tested
// without a cluster.
//
// The stock fakes ignore dryRun (they persist the write regardless), so the
// fake here intercepts every patch: it records it, lets a test inject a
// rejection, and for a dry-run returns the object without persisting — which
// is the property the safety tests need to be able to observe.
package kubetest

import (
	"fmt"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k8s-copilot/internal/kube"
)

// Write is one patch request the fake received.
type Write struct {
	Resource    string
	Namespace   string
	Name        string
	Subresource string
	PatchType   string
	Body        string
	DryRun      bool
}

// Fake is a fake cluster plus what happened to it.
type Fake struct {
	*kube.Cluster
	Dynamic   *dynamicfake.FakeDynamicClient
	Clientset *kubefake.Clientset
	Discovery *Discovery

	mu     sync.Mutex
	writes []Write
	// OnWrite, when set, can reject a patch (dry-run or real) by returning
	// an error.
	OnWrite func(Write) error
	logs    map[string]string
}

// Discovery is a fake whose preferred resources can be set or made to fail.
type Discovery struct {
	*fakediscovery.FakeDiscovery
	Lists []*metav1.APIResourceList
	Err   error
}

func (d *Discovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return d.Lists, d.Err
}

var _ discovery.DiscoveryInterface = (*Discovery)(nil)

// WidgetGVR is a namespaced custom resource served by the fake.
var WidgetGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}

// Resources is what the fake cluster "serves".
func Resources() []*metav1.APIResourceList {
	rw := metav1.Verbs{"get", "list", "watch", "patch", "update", "create", "delete"}
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true, ShortNames: []string{"po"}, Verbs: rw},
			{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"get"}},
			{Name: "services", SingularName: "service", Kind: "Service", Namespaced: true, ShortNames: []string{"svc"}, Verbs: rw},
			{Name: "configmaps", SingularName: "configmap", Kind: "ConfigMap", Namespaced: true, ShortNames: []string{"cm"}, Verbs: rw},
			{Name: "secrets", SingularName: "secret", Kind: "Secret", Namespaced: true, Verbs: rw},
			{Name: "events", SingularName: "event", Kind: "Event", Namespaced: true, ShortNames: []string{"ev"}, Verbs: rw},
			{Name: "namespaces", SingularName: "namespace", Kind: "Namespace", Namespaced: false, ShortNames: []string{"ns"}, Verbs: rw},
			{Name: "nodes", SingularName: "node", Kind: "Node", Namespaced: false, ShortNames: []string{"no"}, Verbs: rw},
			{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: metav1.Verbs{"create"}},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true, ShortNames: []string{"deploy"}, Verbs: rw},
			{Name: "deployments/scale", Kind: "Scale", Namespaced: true, Verbs: metav1.Verbs{"get", "patch", "update"}},
			{Name: "statefulsets", SingularName: "statefulset", Kind: "StatefulSet", Namespaced: true, ShortNames: []string{"sts"}, Verbs: rw},
			{Name: "daemonsets", SingularName: "daemonset", Kind: "DaemonSet", Namespaced: true, ShortNames: []string{"ds"}, Verbs: rw},
			{Name: "replicasets", SingularName: "replicaset", Kind: "ReplicaSet", Namespaced: true, ShortNames: []string{"rs"}, Verbs: rw},
		}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", SingularName: "widget", Kind: "Widget", Namespaced: true, ShortNames: []string{"wd"}, Verbs: rw},
		}},
	}
}

// New builds a fake cluster holding objects. Typed objects go to both
// clients; unstructured ones (custom resources) to the dynamic client only.
func New(objects ...runtime.Object) *Fake {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	var typed []runtime.Object
	for _, o := range objects {
		if _, ok := o.(*unstructured.Unstructured); !ok {
			typed = append(typed, o)
		}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{WidgetGVR: "WidgetList"}, objects...)
	cs := kubefake.NewClientset(typed...)
	disco := &Discovery{FakeDiscovery: cs.Discovery().(*fakediscovery.FakeDiscovery), Lists: Resources()}

	f := &Fake{Dynamic: dyn, Clientset: cs, Discovery: disco, logs: map[string]string{}}
	f.Cluster = kube.NewForClients(kube.Info{
		Context: "test-ctx", Cluster: "test", Server: "https://test.invalid:6443", User: "tester", Namespace: "default",
	}, dyn, cs, disco)

	dyn.PrependReactor("patch", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchActionImpl)
		if !ok {
			return false, nil, nil
		}
		w := Write{
			Resource: pa.GetResource().Resource, Namespace: pa.GetNamespace(), Name: pa.GetName(),
			Subresource: pa.GetSubresource(), PatchType: string(pa.GetPatchType()), Body: string(pa.GetPatch()),
			DryRun: len(pa.PatchOptions.DryRun) > 0,
		}
		f.mu.Lock()
		f.writes = append(f.writes, w)
		hook := f.OnWrite
		f.mu.Unlock()
		if hook != nil {
			if err := hook(w); err != nil {
				return true, nil, err
			}
		}
		if w.DryRun {
			// Validate that the object exists, change nothing.
			obj, err := dyn.Tracker().Get(pa.GetResource(), pa.GetNamespace(), pa.GetName())
			return true, obj, err
		}
		return false, nil, nil // let the tracker apply it
	})

	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GenericActionImpl)
		if !ok || ga.GetSubresource() != "log" {
			return false, nil, nil
		}
		opts, _ := ga.Value.(*corev1.PodLogOptions)
		f.mu.Lock()
		defer f.mu.Unlock()
		key := ga.GetNamespace() + "/"
		if opts != nil {
			key += opts.Container
			if opts.Previous {
				key += "/previous"
			}
		}
		return true, &runtime.Unknown{Raw: []byte(f.logs[key])}, nil
	})
	return f
}

// SetLogs sets the log output for a container in a namespace (any pod).
// previous selects the terminated instance.
func (f *Fake) SetLogs(namespace, container string, previous bool, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := namespace + "/" + container
	if previous {
		key += "/previous"
	}
	f.logs[key] = text
}

// Writes returns every patch received, dry-runs included.
func (f *Fake) Writes() []Write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Write(nil), f.writes...)
}

// RealWrites returns only the patches that were not dry-runs.
func (f *Fake) RealWrites() []Write {
	var out []Write
	for _, w := range f.Writes() {
		if !w.DryRun {
			out = append(out, w)
		}
	}
	return out
}

// MutatingActions lists every non-read action either client saw, as
// "verb resource[/subresource]". Dry-run patches are excluded. The safety
// tests assert on this to prove nothing was changed.
func (f *Fake) MutatingActions() []string {
	var out []string
	add := func(actions []clienttesting.Action) {
		for _, a := range actions {
			switch a.GetVerb() {
			case "get", "list", "watch":
				continue
			}
			if pa, ok := a.(clienttesting.PatchActionImpl); ok && len(pa.PatchOptions.DryRun) > 0 {
				continue
			}
			s := a.GetVerb() + " " + a.GetResource().Resource
			if sub := a.GetSubresource(); sub != "" {
				s += "/" + sub
			}
			out = append(out, s)
		}
	}
	add(f.Dynamic.Actions())
	add(f.Clientset.Actions())
	return out
}

// --- object builders ----------------------------------------------------------

// Deployment builds an apps/v1 Deployment.
func Deployment(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{"app": name}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example/" + name + ":1"}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: replicas},
	}
}

// Pod builds a running pod with the given containers.
func Pod(namespace, name string, containers ...string) *corev1.Pod {
	if len(containers) == 0 {
		containers = []string{"app"}
	}
	p := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID("uid-" + nameUID(namespace, name)), Labels: map[string]string{"app": name}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c, Image: "example/" + c + ":1"})
	}
	return p
}

func nameUID(ns, name string) string { return fmt.Sprintf("%s-%s", ns, name) }

// Namespace builds a namespace.
func Namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// Event builds an event about a pod.
func Event(namespace, name, podName, typ, reason, message string, at metav1.Time) *corev1.Event {
	return &corev1.Event{
		TypeMeta:       metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
		ObjectMeta:     metav1.ObjectMeta{Namespace: namespace, Name: name},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: namespace, Name: podName, UID: types.UID("uid-" + nameUID(namespace, podName))},
		Type:           typ, Reason: reason, Message: message, LastTimestamp: at,
	}
}

// Widget builds a custom resource.
func Widget(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1", "kind": "Widget",
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"status":   map[string]any{"phase": "Ready"},
	}}
}
