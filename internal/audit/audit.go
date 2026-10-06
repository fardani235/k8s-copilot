// Package audit is the append-only, hash-chained record of every gated action.
//
// The file is JSON Lines. Each entry carries the hash of the previous entry
// and its own hash, so editing, reordering or removing an entry breaks the
// chain and Verify reports it. A small sidecar (<file>.head, see head.go)
// remembers the last sequence number and hash so that chopping whole entries
// off the end is detectable too.
//
// This package deliberately has no function that rewrites, truncates or
// removes the log; internal/guard asserts that at the source level.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Decision values.
const (
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
	DecisionEdited   = "edited"
	DecisionNone     = "none" // no human decision was reached
)

// Outcome values.
const (
	OutcomeApplying       = "applying"         // approved; written BEFORE the request is sent, followed by applied or failed
	OutcomeApplied        = "applied"          // approved and the cluster accepted the change
	OutcomeFailed         = "failed"           // approved, but applying it failed; see Error
	OutcomeDeclined       = "declined"         // rejected by the human; nothing was applied
	OutcomeDryRunRejected = "dry_run_rejected" // the API server refused the dry-run; never shown as approvable
	OutcomeSuperseded     = "superseded"       // the human edited it; the edited proposal has its own entry
	OutcomeCancelled      = "cancelled"        // the turn ended (quit/cancel) while waiting; nothing was applied
)

// Dry-run results.
const (
	DryRunPassed   = "passed"
	DryRunRejected = "rejected"
)

// Target is the resource a proposal is about.
type Target struct {
	Context    string `json:"context"`
	Server     string `json:"server"`
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

// Change is one before→after line of a proposal.
type Change struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// Entry is one gated action, from intent to outcome.
type Entry struct {
	Seq        int64     `json:"seq"`
	Time       time.Time `json:"time"`
	Session    string    `json:"session"`
	ProposalID string    `json:"proposal_id"`

	// Intent is the user's request that led to the proposal, verbatim.
	Intent string `json:"intent"`
	// Model is the provider/model that proposed it.
	Model string `json:"model,omitempty"`
	// ModelReason is the model's own justification. It is unverified text.
	ModelReason string `json:"model_reason,omitempty"`

	Tool    string          `json:"tool"`
	Target  Target          `json:"target"`
	Args    json.RawMessage `json:"args"`
	Changes []Change        `json:"changes,omitempty"`
	// Request is the exact API request that was (or would have been) sent.
	Request string `json:"request,omitempty"`

	DryRun struct {
		Result  string `json:"result"`
		Message string `json:"message,omitempty"`
	} `json:"dry_run"`

	Decision struct {
		Action string     `json:"action"`
		By     string     `json:"by,omitempty"`
		At     *time.Time `json:"at,omitempty"`
	} `json:"decision"`

	Outcome struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
		Detail string `json:"detail,omitempty"`
	} `json:"outcome"`

	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// encode returns the canonical line for e (without trailing newline).
func encode(e Entry) ([]byte, error) { return json.Marshal(e) }

// seal fills in Hash: SHA-256 over the canonical encoding with Hash empty.
// PrevHash is part of that encoding, which is what chains the entries.
func seal(e *Entry) error {
	e.Hash = ""
	b, err := encode(*e)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	e.Hash = hex.EncodeToString(sum[:])
	return nil
}

// Log is an open audit file.
type Log struct {
	path string

	mu     sync.Mutex
	f      *os.File
	size   int64
	seq    int64
	hash   string
	broken error // set once a write fails; see Healthy

	initial Report
	loaded  bool
}

// DefaultPath is $XDG_STATE_HOME/k2stui/audit.jsonl, falling back to
// ~/.local/state/k2stui/audit.jsonl.
func DefaultPath() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "k2stui", "audit.jsonl")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "k2stui-audit.jsonl"
	}
	return filepath.Join(home, ".local", "state", "k2stui", "audit.jsonl")
}

// Open opens (creating if needed) the audit file for appending. Existing
// entries are kept and the chain continues from the last one.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit trail: cannot create directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit trail: cannot open %s for appending: %w", path, err)
	}
	l := &Log{path: path, f: f}
	if err := l.syncTail(); err != nil {
		f.Close()
		return nil, err
	}
	return l, nil
}

// Path returns the file path.
func (l *Log) Path() string { return l.path }

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Healthy returns nil while the trail can be relied on. After any failed
// write it returns that failure forever (for this process): the gate uses
// this to refuse further mutations rather than act unrecorded.
func (l *Log) Healthy() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.broken
}

// syncTail positions seq/hash/size at the current end of the file.
func (l *Log) syncTail() error {
	st, err := l.f.Stat()
	if err != nil {
		return fmt.Errorf("audit trail: %w", err)
	}
	rep, err := Verify(l.path)
	if err != nil {
		return err
	}
	l.size, l.seq, l.hash = st.Size(), rep.LastSeq, rep.LastHash
	if !l.loaded {
		l.initial, l.loaded = rep, true
	}
	return nil
}

// Initial is the verification report taken when the log was opened, so the
// application can warn at start-up if the existing trail is damaged.
func (l *Log) Initial() Report { return l.initial }

// Append seals e (sequence number, previous hash, hash) and writes it
// durably: the call returns only after fsync. On any error the entry must be
// treated as NOT recorded.
func (l *Log) Append(e Entry) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.broken != nil {
		return e, l.broken
	}
	sealed, err := l.append(e)
	if err != nil {
		l.broken = fmt.Errorf("audit trail %s is not writable: %w", l.path, err)
		return e, l.broken
	}
	return sealed, nil
}

func (l *Log) append(e Entry) (Entry, error) {
	// Another k2stui may share the file: serialise on an OS lock and pick up
	// whatever it appended so the chain stays linear.
	unlock, err := lockFile(l.f)
	if err != nil {
		return e, err
	}
	defer unlock()

	st, err := l.f.Stat()
	if err != nil {
		return e, err
	}
	// The path must still be the file we hold open. If it was removed or
	// rotated away, writes would succeed into an unlinked file and vanish.
	onDisk, err := os.Stat(l.path)
	if err != nil {
		return e, fmt.Errorf("the audit file is gone: %w", err)
	}
	if !os.SameFile(st, onDisk) {
		return e, errors.New("the audit file was replaced while k2stui was running")
	}
	if st.Size() != l.size {
		if err := l.syncTail(); err != nil {
			return e, err
		}
	}

	var buf bytes.Buffer
	if l.size > 0 && !endsWithNewline(l.path, l.size) {
		// A previous writer died mid-line. Leave the fragment (Verify will
		// flag it) and start on a clean line.
		buf.WriteByte('\n')
	}

	e.Seq = l.seq + 1
	e.PrevHash = l.hash
	e.Time = e.Time.UTC().Round(0)
	if e.Decision.At != nil {
		t := e.Decision.At.UTC().Round(0)
		e.Decision.At = &t
	}
	if len(e.Args) == 0 {
		e.Args = json.RawMessage("null")
	}
	if err := seal(&e); err != nil {
		return e, err
	}
	line, err := encode(e)
	if err != nil {
		return e, err
	}
	buf.Write(line)
	buf.WriteByte('\n')

	if _, err := l.f.Write(buf.Bytes()); err != nil {
		return e, err
	}
	if err := l.f.Sync(); err != nil {
		return e, err
	}
	l.size += int64(buf.Len())
	l.seq, l.hash = e.Seq, e.Hash

	// Best effort: a stale head is tolerated by Verify (the log being ahead
	// of it is fine; being behind it is the alarm).
	_ = writeHead(l.path, head{Seq: e.Seq, Hash: e.Hash})
	return e, nil
}

func endsWithNewline(path string, size int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, size-1); err != nil {
		return true
	}
	return b[0] == '\n'
}

// Problem is one integrity finding.
type Problem struct {
	Line int // 1-based line in the file; 0 for whole-file findings
	Msg  string
}

func (p Problem) String() string {
	if p.Line == 0 {
		return p.Msg
	}
	return fmt.Sprintf("line %d: %s", p.Line, p.Msg)
}

// Report is the result of verifying a log.
type Report struct {
	Entries  []Entry // the entries that parsed, in file order
	Problems []Problem
	Notes    []string // observations that are not integrity failures
	LastSeq  int64
	LastHash string
}

// OK reports whether the chain is intact.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verify reads the whole log and checks every entry's hash, the chain between
// entries, the sequence numbers, and the head anchor. A missing file is an
// empty, valid log. The error return is for I/O failures only; integrity
// findings are in Report.Problems.
func Verify(path string) (Report, error) {
	var rep Report
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, fmt.Errorf("audit trail: cannot read %s: %w", path, err)
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<16)
	lineNo := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			complete := line[len(line)-1] == '\n'
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				// blank separator after a torn write
			case !complete:
				rep.Problems = append(rep.Problems, Problem{lineNo, "incomplete entry at end of file (truncated or interrupted write)"})
			default:
				rep.checkLine(lineNo, line)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return rep, fmt.Errorf("audit trail: cannot read %s: %w", path, err)
		}
	}

	h, ok, err := readHead(path)
	switch {
	case err != nil:
		rep.Problems = append(rep.Problems, Problem{0, "head anchor is unreadable: " + err.Error()})
	case !ok && len(rep.Entries) > 0:
		rep.Problems = append(rep.Problems, Problem{0, "the head anchor file is missing: entries may have been removed from the end of the log"})
	case ok:
		rep.checkHead(h)
	}
	return rep, nil
}

func (rep *Report) checkLine(lineNo int, line []byte) {
	bad := func(msg string) { rep.Problems = append(rep.Problems, Problem{lineNo, msg}) }

	var e Entry
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		bad("not a valid audit entry: " + err.Error())
		return
	}
	// The stored line must be exactly the canonical encoding, so no byte of
	// it can change unnoticed.
	canon, err := encode(e)
	if err != nil || !bytes.Equal(canon, line) {
		bad("entry is not in canonical form (modified?)")
		return
	}
	claimed := e.Hash
	check := e
	if err := seal(&check); err != nil || check.Hash != claimed {
		bad(fmt.Sprintf("entry seq %d: content does not match its hash (modified)", e.Seq))
		// Keep chaining from the claimed hash so one edit is reported once.
	}
	if e.PrevHash != rep.LastHash {
		bad(fmt.Sprintf("entry seq %d: does not follow the previous entry (entries removed, reordered or inserted)", e.Seq))
	}
	if e.Seq != rep.LastSeq+1 {
		bad(fmt.Sprintf("entry seq %d: expected seq %d", e.Seq, rep.LastSeq+1))
	}
	rep.Entries = append(rep.Entries, e)
	rep.LastSeq, rep.LastHash = e.Seq, claimed
}

func (rep *Report) checkHead(h head) {
	if h.Seq > rep.LastSeq {
		rep.Problems = append(rep.Problems, Problem{0, fmt.Sprintf(
			"log ends at seq %d but %d entries were recorded: the newest entries have been removed", rep.LastSeq, h.Seq)})
		return
	}
	for _, e := range rep.Entries {
		if e.Seq == h.Seq {
			if e.Hash != h.Hash {
				rep.Problems = append(rep.Problems, Problem{0, fmt.Sprintf(
					"entry seq %d differs from the one recorded in the head anchor (log rewritten)", h.Seq)})
			}
			return
		}
	}
	if h.Seq > 0 {
		rep.Problems = append(rep.Problems, Problem{0, fmt.Sprintf("entry seq %d named by the head anchor is missing", h.Seq)})
	}
}
