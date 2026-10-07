// Package approval is the human gate between a proposed change and the
// cluster.
//
// The rule it implements: a mutation is applied only when a human has
// explicitly approved that exact change. Three things make that hold:
//
//   - Gate.Ask blocks until the human answers or the turn is cancelled. There
//     is no timer and no default; cancellation yields an error, never a
//     grant.
//   - A Grant can only be created inside this package, and only on the
//     approve branch of Ask.
//   - A Grant is bound to the digest of the request the human was shown, so
//     it cannot be used to apply anything else (see tools.Plan.Apply).
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/fardani235/k8s-copilot/internal/audit"
)

// Action is the human's answer.
type Action int

const (
	Reject Action = iota // the zero value is the safe one
	Approve
	Edit
)

// Decision is what the human chose.
type Decision struct {
	Action Action
	// EditedArgs replaces the tool arguments when Action is Edit. The edited
	// proposal is re-validated, re-dry-run and shown again; nothing is
	// applied on the strength of an edit.
	EditedArgs json.RawMessage
}

// Proposal is everything the human is shown. Every field except ModelReason
// is computed by k8s-copilot from the cluster and the typed arguments, not written
// by the model.
type Proposal struct {
	ID     string
	Tool   string
	Title  string // "Scale Deployment shop/web from 2 to 5 replicas"
	Target audit.Target
	// Changes are the before→after lines.
	Changes []audit.Change
	Args    json.RawMessage
	// Request is the exact API request that will be sent on approval.
	Request string
	Digest  string
	// DryRun is the server's verdict; a Proposal only exists if it passed.
	DryRun string
	// Reversible says whether the change can be put back exactly, and
	// Reversibility says how (or why not).
	Reversible    bool
	Reversibility string
	Warnings      []string
	// Intent is the user request that led here.
	Intent string
	// ModelReason is the model's justification: unverified text.
	ModelReason string
}

// Request is a pending proposal handed to the UI.
type Request struct {
	Proposal Proposal

	once  sync.Once
	reply chan Decision
}

// Decide delivers the human's decision. Only the first call counts.
func (r *Request) Decide(d Decision) {
	r.once.Do(func() { r.reply <- d })
}

// Grant is proof that a human approved one specific request. Its fields are
// unexported and it has no constructor: the only way to obtain one is
// Gate.Ask returning after an Approve.
type Grant struct {
	digest string
}

// Covers reports whether the grant was given for the request with this
// digest. A nil grant covers nothing.
func (g *Grant) Covers(digest string) bool {
	return g != nil && g.digest != "" && g.digest == digest
}

// ErrCancelled is returned by Ask when the turn ends before the human
// answers. Nothing is applied.
var ErrCancelled = errors.New("approval cancelled before a decision was made")

// Gate carries proposals to the UI and decisions back.
type Gate struct {
	requests chan *Request
}

// NewGate returns a gate. The UI must receive from Requests.
func NewGate() *Gate { return &Gate{requests: make(chan *Request)} }

// Requests is the stream of pending proposals for the UI.
func (g *Gate) Requests() <-chan *Request { return g.requests }

// Ask shows p to the human and blocks until they decide. It returns a Grant
// only for Approve. It waits indefinitely: the only other way out is ctx
// being cancelled (the user cancelled the turn or quit), which returns
// ErrCancelled and no grant.
func (g *Gate) Ask(ctx context.Context, p Proposal) (Decision, *Grant, error) {
	req := &Request{Proposal: p, reply: make(chan Decision, 1)}
	select {
	case g.requests <- req:
	case <-ctx.Done():
		return Decision{}, nil, ErrCancelled
	}
	select {
	case d := <-req.reply:
		if d.Action == Approve {
			return d, &Grant{digest: p.Digest}, nil
		}
		return d, nil, nil
	case <-ctx.Done():
		return Decision{}, nil, ErrCancelled
	}
}
