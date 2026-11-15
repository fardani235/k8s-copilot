package tools_test

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/kube/kubetest"
	"github.com/fardani235/k8s-copilot/internal/metrics"
)

// parsed is list_resources' table read back by column position: cells may
// hold spaces ("no lim"), so splitting on whitespace would not do.
type parsed struct {
	cols []string
	rows map[string]map[string]string // NAME → column → cell
}

func parseListing(t *testing.T, out string) parsed {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	hi := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "NAME") || strings.HasPrefix(l, "NAMESPACE ") {
			hi = i
			break
		}
	}
	if hi < 0 {
		t.Fatalf("no table header:\n%s", out)
	}
	header := lines[hi]
	var starts []int
	p := parsed{rows: map[string]map[string]string{}}
	for i := 0; i < len(header); {
		if header[i] == ' ' {
			i++
			continue
		}
		j := strings.IndexByte(header[i:], ' ')
		if j < 0 {
			j = len(header) - i
		}
		starts = append(starts, i)
		p.cols = append(p.cols, header[i:i+j])
		i += j
	}
	for _, l := range lines[hi+1:] {
		// fmt pads in runes, and the header is ASCII: index the line by
		// runes ("—" is one rune, three bytes).
		r := map[string]string{}
		rs := []rune(l)
		for k, s := range starts {
			end := len(rs)
			if k+1 < len(starts) {
				end = min(starts[k+1], len(rs))
			}
			if s < len(rs) {
				r[p.cols[k]] = strings.TrimSpace(string(rs[s:end]))
			}
		}
		p.rows[r["NAME"]] = r
	}
	return p
}

func TestListResourcesCarriesUsage(t *testing.T) {
	f := loadedCluster()
	r := registry(f)
	out, err := r.Read(ctx, "list_resources", args(M{"type": "pods", "namespace": "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[1], "CPU/MEM sampled ") {
		t.Fatalf("no freshness line before the table:\n%s", out)
	}
	p := parseListing(t, out)
	if got := strings.Join(p.cols, " "); got != "NAME STATUS AGE CPU %CPU/R %CPU/L MEM %MEM/R %MEM/L" {
		t.Fatalf("columns %s", got)
	}
	for col, want := range map[string]string{"CPU": "490m", "%CPU/R": "163%", "%CPU/L": "no lim", "MEM": "510Mi", "%MEM/L": "no lim"} {
		if got := p.rows["web-1"][col]; got != want {
			t.Errorf("web-1 %s = %q, want %q", col, got, want)
		}
	}
	// A row without a sample is unknown, never zero.
	for _, col := range []string{"CPU", "MEM", "%CPU/R"} {
		if got := p.rows["web-pending"][col]; got != metrics.NoData {
			t.Errorf("web-pending %s = %q", col, got)
		}
	}

	out, err = r.Read(ctx, "list_resources", args(M{"type": "nodes"}))
	if err != nil {
		t.Fatal(err)
	}
	p = parseListing(t, out)
	if got := strings.Join(p.cols, " "); got != "NAME STATUS AGE CPU %CPU MEM %MEM" {
		t.Fatalf("node columns %s", got)
	}
	if p.rows["node-b"]["CPU"] != "1.9" || p.rows["node-b"]["%CPU"] != "95%" {
		t.Errorf("node-b: %v", p.rows["node-b"])
	}

	// Another type: no line, no columns.
	out, _ = r.Read(ctx, "list_resources", args(M{"type": "namespaces"}))
	if strings.Contains(out, "CPU") {
		t.Errorf("namespaces decorated:\n%s", out)
	}
}

func TestListResourcesSaysWhyUsageIsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    func() *kubetest.Fake
		want string
	}{
		{"absent", func() *kubetest.Fake {
			return kubetest.New(kubetest.Node("node-a", "4", "8Gi"), kubetest.Pod("shop", "web-1"))
		},
			"No CPU/memory columns — Metrics are not available on this cluster. Usage is unknown, not zero."},
		{"unavailable", func() *kubetest.Fake {
			f := loadedCluster()
			f.FailMetrics(apierrors.NewServiceUnavailable("no endpoints"))
			return f
		}, "No CPU/memory columns — The metrics API is not answering. Usage is unknown, not zero."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f()
			for _, typ := range []string{"pods", "nodes"} {
				out, err := registry(f).Read(ctx, "list_resources", args(M{"type": typ, "namespace": "shop"}))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Split(out, "\n")[1] != tc.want {
					t.Errorf("%s: line is not %q:\n%s", typ, tc.want, out)
				}
				if p := parseListing(t, out); strings.Join(p.cols, " ") != "NAME STATUS AGE" {
					t.Errorf("%s: columns %v", typ, p.cols)
				}
				// The browser is handed the very same sentence.
				tbl, err := f.ListWithUsage(ctx, mustResolve(t, f, typ), "shop", 0, kube.MetricsFreshFor)
				if err != nil {
					t.Fatal(err)
				}
				if tbl.Overlay.Note != tc.want {
					t.Errorf("%s: browser note %q", typ, tbl.Overlay.Note)
				}
			}
		})
	}
}

// What the copilot is told equals what the metrics screen shows, cell for
// cell, and it costs no further metrics request while the readings are
// fresh.
func TestListResourcesMatchesTheMetricsScreen(t *testing.T) {
	f := loadedCluster()
	snap := f.Metrics(ctx, kube.AllNamespaces, 0) // what the metrics screen holds
	calls := f.MetricsCalls()
	r := registry(f)

	podsOut, err := r.Read(ctx, "list_resources", args(M{"type": "pods", "namespace": "*"}))
	if err != nil {
		t.Fatal(err)
	}
	nodesOut, err := r.Read(ctx, "list_resources", args(M{"type": "nodes"}))
	if err != nil {
		t.Fatal(err)
	}
	if f.MetricsCalls() != calls {
		t.Fatalf("list_resources asked the metrics API again (%d → %d)", calls, f.MetricsCalls())
	}

	screen := snap.List(metrics.Query{Level: metrics.LevelPods})
	pods := parseListing(t, podsOut)
	compared := 0
	for _, row := range screen.Rows {
		got := pods.rows[row.Name]
		if got == nil {
			t.Fatalf("pod %s missing from list_resources:\n%s", row.Name, podsOut)
		}
		for i, col := range screen.Columns {
			switch col.Name {
			case "CPU", "%CPU/R", "%CPU/L", "MEM", "%MEM/R", "%MEM/L":
				if got[col.Name] != row.Cells[i] {
					t.Errorf("pod %s %s: copilot %q, metrics screen %q", row.Name, col.Name, got[col.Name], row.Cells[i])
				}
				compared++
			}
		}
	}
	nodeScreen := snap.List(metrics.Query{Level: metrics.LevelNodes})
	nodes := parseListing(t, nodesOut)
	for _, row := range nodeScreen.Rows {
		for i, col := range nodeScreen.Columns {
			switch col.Name {
			case "CPU", "%CPU", "MEM", "%MEM":
				if got := nodes.rows[row.Name][col.Name]; got != row.Cells[i] {
					t.Errorf("node %s %s: copilot %q, metrics screen %q", row.Name, col.Name, got, row.Cells[i])
				}
				compared++
			}
		}
	}
	if compared < 6*4+4*2 {
		t.Fatalf("compared only %d cells", compared)
	}
}
