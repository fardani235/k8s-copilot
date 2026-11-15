package metrics_test

import (
	"testing"

	"github.com/fardani235/k8s-copilot/internal/metrics"
)

// The usage cells the ordinary pods and nodes listings borrow are the
// metrics listing's own: these pin what they say in each of the cases that
// must not be confused with one another.
func TestPodUsageCells(t *testing.T) {
	k, mc := metrics.Known, metrics.Millicores
	for _, tc := range []struct {
		name string
		pod  metrics.Pod
		want [6]string
	}{
		{"missing reading", metrics.Pod{BoundsKnown: true, CPUReq: mc(100), MemReq: k(64 << 20)},
			[6]string{"—", "—", "—", "—", "—", "—"}},
		{"measured zero", metrics.Pod{BoundsKnown: true, CPU: k(0), Mem: k(0), CPUReq: mc(100), CPULim: mc(200), MemReq: k(64 << 20), MemLim: k(128 << 20)},
			[6]string{"0m", "0%", "0%", "0", "0%", "0%"}},
		{"bounds known to be absent", metrics.Pod{BoundsKnown: true, CPU: mc(50), Mem: k(32 << 20)},
			[6]string{"50m", "no req", "no lim", "32Mi", "no req", "no lim"}},
		{"bounds unknown", metrics.Pod{BoundsKnown: false, CPU: mc(50), Mem: k(32 << 20)},
			[6]string{"50m", "—", "—", "32Mi", "—", "—"}},
		{"against both bounds", metrics.Pod{BoundsKnown: true, CPU: mc(150), Mem: k(96 << 20), CPUReq: mc(100), CPULim: mc(300), MemReq: k(64 << 20), MemLim: k(128 << 20)},
			[6]string{"150m", "150%", "50%", "96Mi", "150%", "75%"}},
	} {
		if got := metrics.PodUsageCells(&tc.pod); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestNodeUsageCells(t *testing.T) {
	k, mc := metrics.Known, metrics.Millicores
	for _, tc := range []struct {
		name string
		node metrics.Node
		want [4]string
	}{
		{"missing reading", metrics.Node{CPUAlloc: mc(4000), MemAlloc: k(8 << 30)}, [4]string{"—", "—", "—", "—"}},
		{"measured zero", metrics.Node{CPU: k(0), Mem: k(0), CPUAlloc: mc(4000), MemAlloc: k(8 << 30)}, [4]string{"0m", "0%", "0", "0%"}},
		// Every node has an allocatable: one that could not be read is
		// unknown, never "none".
		{"allocatable unknown", metrics.Node{CPU: mc(1200), Mem: k(6 << 30)}, [4]string{"1.2", "—", "6Gi", "—"}},
		{"against allocatable", metrics.Node{CPU: mc(1200), Mem: k(6 << 30), CPUAlloc: mc(4000), MemAlloc: k(8 << 30)}, [4]string{"1.2", "30%", "6Gi", "75%"}},
	} {
		if got := metrics.NodeUsageCells(&tc.node); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The listings are made of those same cells, so the two cannot drift.
func TestListingsUseTheUsageCells(t *testing.T) {
	s := metrics.Build(sample())
	pods := s.List(metrics.Query{Level: metrics.LevelPods})
	for i := range s.Pods {
		p := &s.Pods[i]
		got, want := cells(t, pods, p.Key()), metrics.PodUsageCells(p)
		for j, col := range []string{"CPU", "%CPU/R", "%CPU/L", "MEM", "%MEM/R", "%MEM/L"} {
			if got[col] != want[j] {
				t.Errorf("pod %s %s: listing %q, PodUsageCells %q", p.Key(), col, got[col], want[j])
			}
		}
	}
	nodes := s.List(metrics.Query{Level: metrics.LevelNodes})
	for i := range s.Nodes {
		n := &s.Nodes[i]
		got, want := cells(t, nodes, n.Name), metrics.NodeUsageCells(n)
		for j, col := range []string{"CPU", "%CPU", "MEM", "%MEM"} {
			if got[col] != want[j] {
				t.Errorf("node %s %s: listing %q, NodeUsageCells %q", n.Name, col, got[col], want[j])
			}
		}
	}
}
