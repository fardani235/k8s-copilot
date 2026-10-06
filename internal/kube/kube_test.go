package kube_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	clienttesting "k8s.io/client-go/testing"

	"github.com/fardani235/k2stui/internal/kube"
	"github.com/fardani235/k2stui/internal/kube/kubetest"
)

func writeKubeconfig(t *testing.T, path, current, server string, contexts ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\n")
	if current != "" {
		fmt.Fprintf(&b, "current-context: %s\n", current)
	}
	b.WriteString("clusters:\n")
	for _, c := range contexts {
		fmt.Fprintf(&b, "- name: cluster-%s\n  cluster:\n    server: %s\n", c, strings.ReplaceAll(server, "{ctx}", c))
	}
	b.WriteString("users:\n- name: u\n  user:\n    token: secret-token\ncontexts:\n")
	for _, c := range contexts {
		fmt.Fprintf(&b, "- name: %s\n  context:\n    cluster: cluster-%s\n    user: u\n", c, c)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 1.2: KUBECONFIG precedence, default path, explicit override.
func TestLoadConfigResolution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultPath := filepath.Join(home, ".kube", "config")
	envPath := filepath.Join(t.TempDir(), "env-config")
	writeKubeconfig(t, defaultPath, "home", "https://{ctx}.example:6443", "home", "other")
	writeKubeconfig(t, envPath, "fromenv", "https://{ctx}.example:6443", "fromenv")

	t.Run("default path when KUBECONFIG unset", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")
		_, info, err := kube.LoadConfig(kube.ConnectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if info.Context != "home" || info.Server != "https://home.example:6443" || info.Namespace != "default" {
			t.Fatalf("got %+v", info)
		}
	})
	t.Run("KUBECONFIG wins over default path", func(t *testing.T) {
		t.Setenv("KUBECONFIG", envPath)
		_, info, err := kube.LoadConfig(kube.ConnectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if info.Context != "fromenv" {
			t.Fatalf("got context %q, want fromenv", info.Context)
		}
	})
	t.Run("explicit context override", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")
		_, info, err := kube.LoadConfig(kube.ConnectOptions{Context: "other"})
		if err != nil {
			t.Fatal(err)
		}
		if info.Context != "other" || info.Server != "https://other.example:6443" {
			t.Fatalf("got %+v", info)
		}
	})
	t.Run("unknown context names the available ones", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")
		_, _, err := kube.LoadConfig(kube.ConnectOptions{Context: "nope"})
		var ce *kube.ConfigError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "home, other") {
			t.Fatalf("got %v", err)
		}
	})
}

// 1.3: missing kubeconfig / no usable context.
func TestLoadConfigMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")
	_, _, err := kube.LoadConfig(kube.ConnectOptions{})
	var ce *kube.ConfigError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Fatalf("got %v", err)
	}

	noCurrent := filepath.Join(t.TempDir(), "cfg")
	writeKubeconfig(t, noCurrent, "", "https://x.example", "a", "b")
	_, _, err = kube.LoadConfig(kube.ConnectOptions{Kubeconfig: noCurrent})
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "no current-context") || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("got %v", err)
	}
}

// 1.3: unreachable server and rejected credentials are reported as such,
// with the server address.
func TestConnectFailures(t *testing.T) {
	t.Run("unauthorized", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`)
		}))
		defer srv.Close()
		path := filepath.Join(t.TempDir(), "cfg")
		writeKubeconfig(t, path, "c", srv.URL, "c")

		_, err := kube.Connect(context.Background(), kube.ConnectOptions{Kubeconfig: path})
		var ce *kube.ConnectError
		if !errors.As(err, &ce) {
			t.Fatalf("got %T %v", err, err)
		}
		if !ce.Unauthorized || !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "rejected the credentials") {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(err.Error(), "secret-token") {
			t.Fatal("error leaks the credential")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close() // nothing listens there any more
		path := filepath.Join(t.TempDir(), "cfg")
		writeKubeconfig(t, path, "c", url, "c")

		_, err := kube.Connect(context.Background(), kube.ConnectOptions{Kubeconfig: path})
		var ce *kube.ConnectError
		if !errors.As(err, &ce) || ce.Unauthorized || !strings.Contains(err.Error(), "cannot reach API server "+url) {
			t.Fatalf("got %v", err)
		}
	})
}

// 2.1: discovery yields namespaced, cluster-scoped and custom types; no
// subresources, nothing unlistable.
func TestTypesDiscovery(t *testing.T) {
	f := kubetest.New()
	types, warn, err := f.Types(context.Background(), false)
	if err != nil || warn != nil {
		t.Fatal(err, warn)
	}
	byName := map[string]kube.ResourceType{}
	for _, ty := range types {
		byName[ty.String()] = ty
	}
	for name, namespaced := range map[string]bool{"pods": true, "nodes": false, "namespaces": false, "deployments.apps": true, "widgets.example.com": true} {
		ty, ok := byName[name]
		if !ok {
			t.Fatalf("%s not discovered; got %v", name, types)
		}
		if ty.Namespaced != namespaced {
			t.Errorf("%s namespaced = %v, want %v", name, ty.Namespaced, namespaced)
		}
	}
	for _, bad := range []string{"pods/log", "deployments/scale.apps", "bindings"} {
		if _, ok := byName[bad]; ok {
			t.Errorf("%s must not be browsable", bad)
		}
	}
}

func TestResolve(t *testing.T) {
	f := kubetest.New()
	for in, want := range map[string]string{
		"pods": "pods", "po": "pods", "Pod": "pods", "pod": "pods",
		"deploy": "deployments.apps", "Deployment": "deployments.apps", "deployments.apps": "deployments.apps",
		"wd": "widgets.example.com", "widgets.example.com": "widgets.example.com",
	} {
		got, err := f.Resolve(context.Background(), in)
		if err != nil || got.String() != want {
			t.Errorf("Resolve(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	_, err := f.Resolve(context.Background(), "deploymnts")
	if err == nil || !strings.Contains(err.Error(), "does not serve") {
		t.Fatalf("got %v", err)
	}
}

// Discovery failing outright yields no type list at all; a partial failure
// yields the types plus a warning naming what is missing.
func TestTypesDiscoveryFailure(t *testing.T) {
	f := kubetest.New()
	f.Discovery.Lists, f.Discovery.Err = nil, errors.New("connection refused")
	types, _, err := f.Types(context.Background(), false)
	if err == nil || types != nil {
		t.Fatalf("want error and no types, got %v, %v", types, err)
	}

	f = kubetest.New()
	f.Discovery.Err = &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
		{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("service unavailable"),
	}}
	types, warn, err := f.Types(context.Background(), false)
	if err != nil || len(types) == 0 {
		t.Fatal(types, err)
	}
	if warn == nil || !strings.Contains(warn.Error(), "metrics.k8s.io/v1beta1") {
		t.Fatalf("want a warning naming the failed group, got %v", warn)
	}
}

func resolve(t *testing.T, f *kubetest.Fake, name string) kube.ResourceType {
	t.Helper()
	ty, err := f.Resolve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return ty
}

// 2.2 / 2.3: namespace scoping, all-namespaces rows carry their namespace,
// cluster-scoped types are not namespace-filtered.
func TestListScoping(t *testing.T) {
	f := kubetest.New(
		kubetest.Namespace("shop"), kubetest.Namespace("ops"),
		kubetest.Pod("shop", "web-1"), kubetest.Pod("shop", "web-2"), kubetest.Pod("ops", "cron-1"),
		kubetest.Widget("shop", "w1"),
	)
	ctx := context.Background()

	nss, err := f.Namespaces(ctx)
	if err != nil || strings.Join(nss, ",") != "ops,shop" {
		t.Fatal(nss, err)
	}

	pods := resolve(t, f, "pods")
	tbl, err := f.List(ctx, pods, "shop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 2 || tbl.Columns[0] != "NAME" {
		t.Fatalf("got %+v", tbl)
	}
	for _, r := range tbl.Rows {
		if r.Namespace != "shop" {
			t.Fatalf("row from another namespace: %+v", r)
		}
	}

	all, err := f.List(ctx, pods, kube.AllNamespaces, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Rows) != 3 || all.Columns[0] != "NAMESPACE" {
		t.Fatalf("got %+v", all)
	}
	for _, r := range all.Rows {
		if r.Namespace == "" || r.Cells[0] != r.Namespace {
			t.Fatalf("all-namespaces row does not show its namespace: %+v", r)
		}
	}
	wantCols := "NAMESPACE NAME STATUS AGE"
	if got := strings.Join(all.Columns, " "); got != wantCols {
		t.Fatalf("columns %q, want %q", got, wantCols)
	}
	if all.Rows[0].Cells[2] != "Running" {
		t.Fatalf("status cell = %q", all.Rows[0].Cells[2])
	}

	// Cluster-scoped: the namespace argument must not filter or add a column.
	nsType := resolve(t, f, "namespaces")
	nst, err := f.List(ctx, nsType, "shop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nst.Rows) != 2 || nst.Columns[0] != "NAME" {
		t.Fatalf("cluster-scoped listing was namespace-filtered: %+v", nst)
	}

	// A custom resource lists like anything else.
	wt, err := f.List(ctx, resolve(t, f, "widgets"), "shop", 0)
	if err != nil || len(wt.Rows) != 1 || wt.Rows[0].Cells[1] != "Ready" {
		t.Fatalf("%+v %v", wt, err)
	}
}

// 2.4: empty and forbidden are different, explicit results.
func TestListEmptyAndForbidden(t *testing.T) {
	f := kubetest.New()
	pods := resolve(t, f, "pods")
	tbl, err := f.List(context.Background(), pods, "default", 0)
	if err != nil || len(tbl.Rows) != 0 {
		t.Fatal(tbl, err)
	}

	f.Dynamic.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("user tester cannot list pods"))
	})
	_, err = f.List(context.Background(), pods, "default", 0)
	if !kube.IsDenied(err) {
		t.Fatalf("want a permission error, got %v", err)
	}
	if !strings.Contains(kube.Reason(err), "cannot list pods") {
		t.Fatalf("reason lost: %q", kube.Reason(err))
	}
}

// 2.3: server-side Table columns are used when the server offers them, and
// the namespace column is added in all-namespaces mode.
func TestListServerTable(t *testing.T) {
	var gotPath, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAccept = r.URL.Path, r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/version":
			io.WriteString(w, `{"gitVersion":"v1.35.0"}`)
		case strings.HasSuffix(r.URL.Path, "/secrets"):
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"secrets is forbidden: User \"u\" cannot list resource \"secrets\"","reason":"Forbidden","code":403}`)
		default:
			io.WriteString(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
			 "columnDefinitions":[{"name":"Name","type":"string","priority":0},{"name":"Ready","type":"string","priority":0},
			   {"name":"Status","type":"string","priority":0},{"name":"Restarts","type":"integer","priority":0},
			   {"name":"Age","type":"string","priority":0},{"name":"IP","type":"string","priority":1}],
			 "rows":[{"cells":["web-1","0/1","CrashLoopBackOff",7,"5m","10.0.0.4"],
			   "object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":{"name":"web-1","namespace":"shop","creationTimestamp":"2026-01-01T00:00:00Z"}}}]}`)
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "cfg")
	writeKubeconfig(t, path, "c", srv.URL, "c")
	c, err := kube.Connect(context.Background(), kube.ConnectOptions{Kubeconfig: path})
	if err != nil {
		t.Fatal(err)
	}
	pods := kube.ResourceType{Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true}

	tbl, err := c.List(context.Background(), pods, kube.AllNamespaces, 0)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/pods" || !strings.Contains(gotAccept, "as=Table") {
		t.Fatalf("request was %s (Accept %s)", gotPath, gotAccept)
	}
	if got := strings.Join(tbl.Columns, " "); got != "NAMESPACE NAME READY STATUS RESTARTS AGE" {
		t.Fatalf("columns: %q", got)
	}
	if got := strings.Join(tbl.Rows[0].Cells, " "); got != "shop web-1 0/1 CrashLoopBackOff 7 5m" {
		t.Fatalf("cells: %q", got)
	}
	if !tbl.ServerColumns || tbl.Rows[0].Name != "web-1" || tbl.Rows[0].Namespace != "shop" {
		t.Fatalf("%+v", tbl)
	}

	if _, err := c.List(context.Background(), pods, "shop", 0); err != nil || gotPath != "/api/v1/namespaces/shop/pods" {
		t.Fatalf("namespaced path = %s, err %v", gotPath, err)
	}

	secrets := kube.ResourceType{Version: "v1", Resource: "secrets", Kind: "Secret", Namespaced: true}
	_, err = c.List(context.Background(), secrets, "shop", 0)
	if !kube.IsDenied(err) {
		t.Fatalf("a 403 must surface as a permission error, got %v", err)
	}
}

// 3.2: events for one resource, most recent last.
func TestEventsOrderAndFilter(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(d)) }
	f := kubetest.New(
		kubetest.Event("shop", "e3", "web-1", "Warning", "BackOff", "third", at(-1*time.Minute)),
		kubetest.Event("shop", "e1", "web-1", "Normal", "Scheduled", "first", at(-10*time.Minute)),
		kubetest.Event("shop", "e2", "web-1", "Normal", "Pulled", "second", at(-5*time.Minute)),
		kubetest.Event("shop", "other", "web-2", "Normal", "Pulled", "not mine", at(-2*time.Minute)),
	)
	evs, err := f.Events(context.Background(), kube.ObjectRef{Kind: "Pod", Namespace: "shop", Name: "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.Message)
	}
	if strings.Join(got, ",") != "first,second,third" {
		t.Fatalf("got %v, want oldest first and only web-1's", got)
	}
}

// 3.1: log retrieval, per-container, and the empty case.
func TestLogs(t *testing.T) {
	f := kubetest.New(kubetest.Pod("shop", "web-1", "app", "sidecar"))
	f.SetLogs("shop", "app", false, "app line 1\napp line 2\n")
	f.SetLogs("shop", "sidecar", false, "sidecar line\n")

	read := func(o kube.LogOptions) string {
		rc, err := f.Logs(context.Background(), "shop", "web-1", o)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		return string(b)
	}
	if got := read(kube.LogOptions{Container: "app"}); got != "app line 1\napp line 2\n" {
		t.Fatalf("app: %q", got)
	}
	if got := read(kube.LogOptions{Container: "sidecar"}); got != "sidecar line\n" {
		t.Fatalf("sidecar: %q", got)
	}
	if got := read(kube.LogOptions{Container: "app", Previous: true}); got != "" {
		t.Fatalf("previous should be empty, got %q", got)
	}

	pod, err := f.Get(context.Background(), resolve(t, f, "pods"), "shop", "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kube.Containers(pod), ","); got != "app,sidecar" {
		t.Fatalf("containers: %s", got)
	}
}

func TestSummarizeAndYAML(t *testing.T) {
	f := kubetest.New(kubetest.Deployment("shop", "web", 3))
	o, err := f.Get(context.Background(), resolve(t, f, "deploy"), "shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, fld := range kube.Summarize(o, time.Now()) {
		text.WriteString(fld.Key + ": " + fld.Value + "\n")
	}
	for _, want := range []string{"Kind: Deployment (apps/v1)", "Name: web", "Namespace: shop", "Replicas: desired=3, ready=3", "Images: app=example/web:1"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, text.String())
		}
	}
	y := kube.YAML(o)
	if !strings.Contains(y, "kind: Deployment") || !strings.Contains(y, "replicas: 3") || strings.Contains(y, "managedFields") {
		t.Fatalf("yaml:\n%s", y)
	}
}
