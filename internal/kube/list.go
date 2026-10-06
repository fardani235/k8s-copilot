package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/duration"
)

// AllNamespaces is the namespace value meaning "every namespace".
const AllNamespaces = ""

// DefaultListLimit caps one listing. A truncated listing says so (Table.More).
const DefaultListLimit = 500

// Table is a kubectl-style listing.
type Table struct {
	Type    ResourceType
	Columns []string
	Rows    []Row
	// More is true when the server had more rows than the limit.
	More bool
	// ServerColumns is true when the columns came from the API server's own
	// printer (the same ones kubectl shows), false for the generic fallback.
	ServerColumns bool
}

// Row is one resource in a Table. Cells line up with Table.Columns.
type Row struct {
	Namespace string
	Name      string
	Created   time.Time
	Cells     []string
}

// Namespaces lists the namespaces the caller can see, sorted.
func (c *Cluster) Namespaces(ctx context.Context) ([]string, error) {
	l, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(l.Items))
	for _, n := range l.Items {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out, nil
}

// List lists resources of type t. For a namespaced type, namespace scopes the
// listing and AllNamespaces lists everywhere (each row then carries a
// NAMESPACE column). For a cluster-scoped type the namespace is ignored.
func (c *Cluster) List(ctx context.Context, t ResourceType, namespace string, limit int) (*Table, error) {
	if !t.Namespaced {
		namespace = AllNamespaces
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	showNS := t.Namespaced && namespace == AllNamespaces

	if c.http != nil {
		tbl, err := c.listServerTable(ctx, t, namespace, limit, showNS)
		if err == nil {
			return tbl, nil
		}
		if !apierrors.IsNotAcceptable(err) && !isNoTable(err) {
			return nil, err
		}
		// Server cannot print this type as a Table: fall through.
	}
	return c.listGeneric(ctx, t, namespace, limit, showNS)
}

type noTableError struct{ why string }

func (e *noTableError) Error() string { return e.why }

func isNoTable(err error) bool { _, ok := err.(*noTableError); return ok }

// listServerTable asks the API server to render the listing as a
// meta.k8s.io/v1 Table. This is where kubectl's columns come from, so every
// type — including CRDs with additionalPrinterColumns — gets the columns its
// authors defined, with no per-kind code here.
func (c *Cluster) listServerTable(ctx context.Context, t ResourceType, namespace string, limit int, showNS bool) (*Table, error) {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + resourcePath(t, namespace)
	u.RawQuery = url.Values{"limit": {strconv.Itoa(limit)}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, body, t)
	}

	var st metav1.Table
	if err := json.Unmarshal(body, &st); err != nil || st.Kind != "Table" {
		return nil, &noTableError{why: "server did not return a Table"}
	}

	out := &Table{Type: t, More: st.Continue != "", ServerColumns: true}
	var keep []int
	if showNS {
		out.Columns = append(out.Columns, "NAMESPACE")
	}
	for i, col := range st.ColumnDefinitions {
		if col.Priority != 0 { // "wide" columns
			continue
		}
		keep = append(keep, i)
		out.Columns = append(out.Columns, strings.ToUpper(col.Name))
	}
	for _, r := range st.Rows {
		var meta struct {
			Metadata struct {
				Name              string      `json:"name"`
				Namespace         string      `json:"namespace"`
				CreationTimestamp metav1.Time `json:"creationTimestamp"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(r.Object.Raw, &meta)
		row := Row{Namespace: meta.Metadata.Namespace, Name: meta.Metadata.Name, Created: meta.Metadata.CreationTimestamp.Time}
		if showNS {
			row.Cells = append(row.Cells, row.Namespace)
		}
		for _, i := range keep {
			cell := ""
			if i < len(r.Cells) {
				cell = cellString(r.Cells[i])
			}
			row.Cells = append(row.Cells, cell)
		}
		if row.Name == "" && len(keep) > 0 && len(r.Cells) > 0 {
			row.Name = cellString(r.Cells[0])
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

func cellString(v any) string {
	switch x := v.(type) {
	case nil:
		return "<none>"
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func resourcePath(t ResourceType, namespace string) string {
	var b strings.Builder
	if t.Group == "" {
		b.WriteString("/api/" + t.Version)
	} else {
		b.WriteString("/apis/" + t.Group + "/" + t.Version)
	}
	if t.Namespaced && namespace != AllNamespaces {
		b.WriteString("/namespaces/" + url.PathEscape(namespace))
	}
	b.WriteString("/" + t.Resource)
	return b.String()
}

func statusError(code int, body []byte, t ResourceType) error {
	var st metav1.Status
	if json.Unmarshal(body, &st) == nil && st.Kind == "Status" && st.Code != 0 {
		return apierrors.FromObject(&st)
	}
	return apierrors.NewGenericServerResponse(code, "GET", t.GVR().GroupResource(), "", strings.TrimSpace(string(body)), 0, true)
}

// listGeneric is the safe fallback: NAME, STATUS (best effort), AGE.
func (c *Cluster) listGeneric(ctx context.Context, t ResourceType, namespace string, limit int, showNS bool) (*Table, error) {
	l, err := c.dyn.Resource(t.GVR()).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := &Table{Type: t, More: l.GetContinue() != ""}
	if showNS {
		out.Columns = append(out.Columns, "NAMESPACE")
	}
	out.Columns = append(out.Columns, "NAME", "STATUS", "AGE")
	now := time.Now()
	for i := range l.Items {
		o := &l.Items[i]
		row := Row{Namespace: o.GetNamespace(), Name: o.GetName(), Created: o.GetCreationTimestamp().Time}
		if showNS {
			row.Cells = append(row.Cells, row.Namespace)
		}
		row.Cells = append(row.Cells, row.Name, StatusOf(o), Age(row.Created, now))
		out.Rows = append(out.Rows, row)
	}
	sort.SliceStable(out.Rows, func(i, j int) bool {
		if out.Rows[i].Namespace != out.Rows[j].Namespace {
			return out.Rows[i].Namespace < out.Rows[j].Namespace
		}
		return out.Rows[i].Name < out.Rows[j].Name
	})
	return out, nil
}

// Get fetches one resource.
func (c *Cluster) Get(ctx context.Context, t ResourceType, namespace, name string) (*unstructured.Unstructured, error) {
	if !t.Namespaced {
		namespace = ""
	}
	return c.dyn.Resource(t.GVR()).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// Age formats a creation time the way kubectl does ("5d", "3h12m").
func Age(created, now time.Time) string {
	if created.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(now.Sub(created))
}

// StatusOf derives a short status for an arbitrary object: its phase, its
// ready/desired replica count, or its Ready/Available condition. It returns
// "" when the object carries none of those.
func StatusOf(o *unstructured.Unstructured) string {
	if o.GetDeletionTimestamp() != nil {
		return "Terminating"
	}
	if phase, ok, _ := unstructured.NestedString(o.Object, "status", "phase"); ok && phase != "" {
		return phase
	}
	if desired, ok, _ := unstructured.NestedInt64(o.Object, "spec", "replicas"); ok {
		ready, _, _ := unstructured.NestedInt64(o.Object, "status", "readyReplicas")
		return fmt.Sprintf("%d/%d ready", ready, desired)
	}
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, want := range []string{"Ready", "Available", "Established", "Complete"} {
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if !ok || m["type"] != want {
				continue
			}
			if m["status"] == "True" {
				return want
			}
			if r, _ := m["reason"].(string); r != "" {
				return "Not" + want + " (" + r + ")"
			}
			return "Not" + want
		}
	}
	return ""
}
