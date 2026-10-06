package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample(tool, outcome string) Entry {
	e := Entry{
		Time: time.Now(), Session: "s1", ProposalID: "p1", Intent: "why is web slow?",
		Tool: tool, Target: Target{Context: "c", Server: "https://s", APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Args:    json.RawMessage(`{"replicas": 3}`),
		Changes: []Change{{Field: "spec.replicas", Before: "2", After: "3"}},
	}
	e.DryRun.Result = DryRunPassed
	e.Decision.Action = DecisionApproved
	e.Outcome.Status = outcome
	return e
}

func openLog(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func mustVerify(t *testing.T, path string) Report {
	t.Helper()
	rep, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestAppendChainsAndVerifies(t *testing.T) {
	l, path := openLog(t)
	var prev string
	for i, outcome := range []string{OutcomeApplied, OutcomeDeclined, OutcomeFailed} {
		e, err := l.Append(sample("scale", outcome))
		if err != nil {
			t.Fatal(err)
		}
		if e.Seq != int64(i+1) || e.PrevHash != prev || len(e.Hash) != 64 {
			t.Fatalf("entry %d: seq=%d prev=%q hash=%q", i, e.Seq, e.PrevHash, e.Hash)
		}
		prev = e.Hash
	}
	rep := mustVerify(t, path)
	if !rep.OK() || len(rep.Entries) != 3 || len(rep.Notes) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if got := rep.Entries[1]; got.Outcome.Status != OutcomeDeclined || got.Intent != "why is web slow?" || string(got.Args) != `{"replicas":3}` {
		t.Fatalf("entry did not round-trip: %+v", got)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("audit file mode %v, want 0600", st.Mode().Perm())
	}
}

// 7.3: entries survive a restart and the chain continues across it.
func TestPersistsAcrossRestart(t *testing.T) {
	l, path := openLog(t)
	first, _ := l.Append(sample("scale", OutcomeApplied))
	l.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if !l2.Initial().OK() || len(l2.Initial().Entries) != 1 {
		t.Fatalf("initial report: %+v", l2.Initial())
	}
	second, err := l2.Append(sample("set_labels", OutcomeDeclined))
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq != 2 || second.PrevHash != first.Hash {
		t.Fatalf("chain did not continue: %+v", second)
	}
	if rep := mustVerify(t, path); !rep.OK() || len(rep.Entries) != 2 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestTwoWritersKeepOneChain(t *testing.T) {
	l1, path := openLog(t)
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	for i := 0; i < 5; i++ {
		if _, err := l1.Append(sample("scale", OutcomeApplied)); err != nil {
			t.Fatal(err)
		}
		if _, err := l2.Append(sample("scale", OutcomeDeclined)); err != nil {
			t.Fatal(err)
		}
	}
	if rep := mustVerify(t, path); !rep.OK() || len(rep.Entries) != 10 {
		t.Fatalf("report: %+v", rep.Problems)
	}
}

// 7.2: every way of tampering is detected.
func TestTamperingIsDetected(t *testing.T) {
	build := func(t *testing.T) (string, []string) {
		l, path := openLog(t)
		for _, o := range []string{OutcomeApplied, OutcomeDeclined, OutcomeApplied} {
			if _, err := l.Append(sample("scale", o)); err != nil {
				t.Fatal(err)
			}
		}
		l.Close()
		data, _ := os.ReadFile(path)
		return path, strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	write := func(t *testing.T, path string, lines ...string) {
		t.Helper()
		out := strings.Join(lines, "")
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, path string, lines []string){
		"edited field": func(t *testing.T, path string, lines []string) {
			write(t, path, lines[0], strings.Replace(lines[1], `"declined"`, `"applied"`, 1), lines[2])
		},
		"edited args": func(t *testing.T, path string, lines []string) {
			write(t, path, strings.Replace(lines[0], `{"replicas":3}`, `{"replicas":9}`, 1), lines[1], lines[2])
		},
		"entry removed from the middle": func(t *testing.T, path string, lines []string) {
			write(t, path, lines[0], lines[2])
		},
		"entries reordered": func(t *testing.T, path string, lines []string) {
			write(t, path, lines[1], lines[0], lines[2])
		},
		"newest entry removed": func(t *testing.T, path string, lines []string) {
			write(t, path, lines[0], lines[1])
		},
		"everything removed": func(t *testing.T, path string, lines []string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"last entry cut short": func(t *testing.T, path string, lines []string) {
			data, _ := os.ReadFile(path)
			if err := os.WriteFile(path, data[:len(data)-40], 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"extra field smuggled in": func(t *testing.T, path string, lines []string) {
			write(t, path, lines[0], lines[1], strings.Replace(lines[2], `{"seq":3,`, `{"seq":3,"note":"x",`, 1))
		},
		"whitespace changed": func(t *testing.T, path string, lines []string) {
			write(t, path, lines[0], strings.Replace(lines[1], `{"seq":2,`, `{"seq": 2,`, 1), lines[2])
		},
		"forged entry appended": func(t *testing.T, path string, lines []string) {
			forged := sample("scale", OutcomeApplied)
			forged.Seq, forged.PrevHash, forged.Hash = 4, "deadbeef", "deadbeef"
			b, _ := json.Marshal(forged)
			write(t, path, lines[0], lines[1], lines[2], string(b)+"\n")
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			path, lines := build(t)
			if rep := mustVerify(t, path); !rep.OK() {
				t.Fatalf("log is bad before tampering: %v", rep.Problems)
			}
			tamper(t, path, lines)
			rep := mustVerify(t, path)
			if rep.OK() {
				t.Fatalf("tampering (%s) went undetected", name)
			}
			t.Log(rep.Problems)
		})
	}
}

// Appending after someone damaged the file keeps working and keeps the
// damage visible.
func TestAppendAfterTornWrite(t *testing.T) {
	l, path := openLog(t)
	l.Append(sample("scale", OutcomeApplied))
	l.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"seq":2,"time":"2026-`) // a write that died mid-line
	f.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.Initial().OK() {
		t.Fatal("torn line not reported at open")
	}
	e, err := l2.Append(sample("scale", OutcomeDeclined))
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 2 {
		t.Fatalf("seq %d", e.Seq)
	}
	rep := mustVerify(t, path)
	if rep.OK() || len(rep.Entries) != 2 || len(rep.Problems) != 1 {
		t.Fatalf("want 2 good entries and exactly the torn line flagged, got %d entries, problems %v", len(rep.Entries), rep.Problems)
	}
}

// 7.3: an unwritable trail is an error, stays an error, and is never
// reported as a successful record.
func TestUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, err := Open(filepath.Join(dir, "sub", "audit.jsonl")); err == nil {
		t.Fatal("Open succeeded in a read-only directory")
	}

	l, _ := openLog(t)
	if err := l.Healthy(); err != nil {
		t.Fatal(err)
	}
	l.f.Close() // the storage goes away underneath us
	if _, err := l.Append(sample("scale", OutcomeApplied)); err == nil {
		t.Fatal("Append reported success on a closed file")
	}
	if l.Healthy() == nil {
		t.Fatal("log still claims to be healthy after a failed write")
	}
	if _, err := l.Append(sample("scale", OutcomeApplied)); err == nil {
		t.Fatal("a broken log must keep failing")
	}
}

// A log that was deleted or swapped out from under a running process must
// not keep "succeeding" into a file nobody will ever read.
func TestReplacedFileIsDetected(t *testing.T) {
	l, path := openLog(t)
	if _, err := l.Append(sample("scale", OutcomeApplied)); err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	if _, err := l.Append(sample("scale", OutcomeApplied)); err == nil || l.Healthy() == nil {
		t.Fatal("append succeeded into a deleted file")
	}

	l2, path2 := openLog(t)
	l2.Append(sample("scale", OutcomeApplied))
	os.Rename(path2, path2+".1")
	os.WriteFile(path2, nil, 0o600)
	if _, err := l2.Append(sample("scale", OutcomeApplied)); err == nil {
		t.Fatal("append succeeded after the file was rotated away")
	}
}

// Cutting entries off the end AND deleting the head anchor is still caught.
func TestTruncationWithHeadRemoved(t *testing.T) {
	l, path := openLog(t)
	for i := 0; i < 3; i++ {
		l.Append(sample("scale", OutcomeApplied))
	}
	l.Close()
	data, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(data), "\n")
	os.WriteFile(path, []byte(lines[0]+lines[1]), 0o600)
	os.Remove(path + ".head")
	if rep := mustVerify(t, path); rep.OK() {
		t.Fatal("truncation with the head anchor removed went undetected")
	}
}

func TestMissingFileIsEmptyValidLog(t *testing.T) {
	rep, err := Verify(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || !rep.OK() || len(rep.Entries) != 0 {
		t.Fatal(rep, err)
	}
}
