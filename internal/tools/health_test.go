package tools_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fardani235/k8s-copilot/internal/health"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

func crashLooping(ns, name string, restarts int32) *corev1.Pod {
	p := kubetest.Pod(ns, name)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: restarts,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}}}}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(time.Now().Add(-4 * time.Minute))}}
	return p
}

// 3.1: the tool runs at once, reads only, and prints the health model's own
// report: every finding with class, object, reason and age, and every
// class's state.
func TestGetFindings(t *testing.T) {
	f := kubetest.New(kubetest.Node("node-a", "4", "8Gi"), crashLooping("shop", "web-1", 7), kubetest.Pod("shop", "ok-1"))
	r := registry(f)
	spec, ok := r.Lookup("get_findings")
	if !ok || spec.Tier != tools.Read {
		t.Fatalf("get_findings: %+v", spec)
	}
	out, err := r.Read(ctx, "get_findings", args(M{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"These are the same findings the user sees on the health screen",
		"- [workloads, critical] Pod shop/web-1 container app — CrashLoopBackOff, for 4m",
		"workloads: watched, 1 finding.",
		"nodes: watched, no findings.",
		// No metrics API on the fake: not watched, never "nothing hot".
		"usage: NOT WATCHED (not applicable)",
		"NOT watched: usage (not applicable) — problems there are unknown, not absent.",
		"Covers: containers in CrashLoopBackOff or restarted more than 5 times",
		"Warning events in the last 15m",
		"Never describe the cluster as healthy in a respect that was not watched",
		"Not covered by any class",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	if got := f.MutatingActions(); len(got) != 0 {
		t.Fatalf("get_findings sent %v", got)
	}
	// It is the same report the pure model makes of the same observation.
	obs := f.HealthObservation(ctx, kube.AnyFallback, kube.HealthFreshFor)
	if want := tools.FindingsReport(health.Observe(obs), time.Now(), tools.DefaultOptions().MaxResultBytes); stripAge(want) != stripAge(out) {
		t.Fatalf("the tool's report differs from the model's:\n%s\n---\n%s", out, want)
	}

	// A bad namespace is refused before anything is read.
	if _, err := r.Read(ctx, "get_findings", args(M{"namespace": "Not_A_Namespace"})); err == nil || !strings.Contains(err.Error(), "not a valid namespace name") {
		t.Fatalf("bad namespace: %v", err)
	}
	if _, err := r.Read(ctx, "get_findings", args(M{"level": "x"})); err == nil {
		t.Fatal("unknown argument accepted")
	}
}

// stripAge removes the one line that depends on the clock.
func stripAge(s string) string {
	_, rest, _ := strings.Cut(s, "\n")
	return rest
}

// Everything watched and nothing wrong: the result says each class was
// watched and claims nothing beyond that.
func TestGetFindingsAllClear(t *testing.T) {
	f := kubetest.New(kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "ok-1"))
	f.SetNodeMetrics("node-a", "400m", "2Gi")
	f.SetPodMetrics("shop", "ok-1", kubetest.Usage{Container: "app", CPU: "10m", Memory: "20Mi"})
	out, err := registry(f).Read(ctx, "get_findings", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"No findings. Every class was watched across the whole cluster and none reported a problem: workloads, nodes, rollouts, usage, events.", "Not covered by any class"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
}

// 3.1: too many findings for one result: fewer findings, how many of how
// many — and every class's state still there.
func TestFindingsCapKeepsClassStates(t *testing.T) {
	var objs []runtime.Object
	for i := 0; i < 300; i++ {
		objs = append(objs, crashLooping("shop", fmt.Sprintf("web-%03d", i), 9))
	}
	f := kubetest.New(objs...)
	opts := tools.DefaultOptions()
	opts.MaxResultBytes = 6000
	r := tools.NewRegistry(f.Cluster, opts)
	out, err := r.Read(ctx, "get_findings", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > opts.MaxResultBytes {
		t.Fatalf("%d bytes, cap %d", len(out), opts.MaxResultBytes)
	}
	rep := health.Observe(f.HealthObservation(ctx, kube.AnyFallback, kube.HealthFreshFor))
	for i := range rep.Classes {
		if !strings.Contains(out, rep.Classes[i].Line()) {
			t.Errorf("the capped result lost %s's state:\n%s", rep.Classes[i].ID, out)
		}
	}
	if !strings.Contains(out, "of 300 to fit; every class's state above is complete. Not shown: workloads") {
		t.Errorf("the cut is not stated:\n%s", out)
	}
	if strings.Contains(out, "more bytes)") {
		t.Error("the result was cut mid-text by the generic byte cap")
	}
}
