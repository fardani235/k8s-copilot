package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fardani235/k2stui/internal/approval"
	"github.com/fardani235/k2stui/internal/audit"
	"github.com/fardani235/k2stui/internal/kube"
	"github.com/fardani235/k2stui/internal/llm"
	"github.com/fardani235/k2stui/internal/tools"
)

// DeclinedByUser is the tool result for a rejected proposal.
const DeclinedByUser = "declined by user: the user reviewed this proposal and said no. Nothing was applied. Do not propose it again unless the user asks; continue without it."

// mutate takes a mutate tool call through plan → dry-run → human gate →
// apply. Every way it can end writes an audit entry; an approval writes one
// before the request is sent and one with the result. It is the only caller
// of Plan.Apply, and it only calls it with the grant the gate returned.
func (a *Agent) mutate(ctx context.Context, call llm.ToolCall, intent string, emit func(Event)) (string, bool) {
	// No record, no change: without a working audit trail nothing is even
	// proposed.
	if a.cfg.Audit == nil {
		return "refused: the audit trail is not available, so k2stui will not propose changes. Tell the user; the read tools still work.", true
	}
	if err := a.cfg.Audit.Healthy(); err != nil {
		return fmt.Sprintf("refused: %v, so k2stui will not propose changes. Tell the user; the read tools still work.", err), true
	}

	args := call.Args
	edited := false
	for {
		pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		plan, err := a.cfg.Registry.Plan(pctx, call.Name, args)
		if err == nil {
			err = plan.DryRun(pctx)
			if err != nil {
				cancel()
				return a.dryRunRejected(plan, err, intent, edited, emit)
			}
		}
		cancel()
		if err != nil {
			if edited {
				return "the user edited the proposal, but the edited version is not valid, so nothing was applied: " + err.Error(), true
			}
			return err.Error(), true
		}

		proposal, err := plan.Proposal(intent)
		if err != nil {
			return err.Error(), true
		}
		entry := a.entry(plan, intent)
		entry.DryRun.Result = audit.DryRunPassed

		// Blocks until the human answers. No timeout, no default.
		decision, grant, err := a.cfg.Gate.Ask(ctx, proposal)
		if err != nil {
			entry.Decision.Action = audit.DecisionNone
			entry.Outcome.Status = audit.OutcomeCancelled
			entry.Outcome.Detail = "the request ended before the user decided; nothing was applied"
			audited := a.record(entry, emit)
			emit(EventProposalClosed{ID: plan.ID, Title: plan.Title, Outcome: audit.OutcomeCancelled, Audited: audited})
			return "cancelled: the user ended the request before deciding. Nothing was applied.", true
		}
		now := time.Now()
		entry.Decision.By, entry.Decision.At = a.cfg.Approver, &now

		switch decision.Action {
		case approval.Approve:
			return a.apply(ctx, plan, grant, entry, emit)

		case approval.Edit:
			entry.Decision.Action = audit.DecisionEdited
			entry.Outcome.Status = audit.OutcomeSuperseded
			entry.Outcome.Detail = "the user edited the arguments; the edited proposal, if valid, is the next entry"
			audited := a.record(entry, emit)
			emit(EventProposalClosed{ID: plan.ID, Title: plan.Title, Outcome: audit.OutcomeSuperseded, Audited: audited})
			if !audited {
				return "the user edited the proposal, but the audit trail could not be written, so nothing was applied.", true
			}
			args, edited = decision.EditedArgs, true
			continue

		default: // Reject, and anything unexpected, means no.
			entry.Decision.Action = audit.DecisionRejected
			entry.Outcome.Status = audit.OutcomeDeclined
			entry.Outcome.Detail = "nothing was applied"
			audited := a.record(entry, emit)
			emit(EventProposalClosed{ID: plan.ID, Title: plan.Title, Outcome: audit.OutcomeDeclined, Audited: audited})
			return DeclinedByUser, false
		}
	}
}

func (a *Agent) entry(p *tools.Plan, intent string) audit.Entry {
	e := audit.Entry{
		Time: time.Now(), Session: a.cfg.Session, ProposalID: p.ID,
		Intent: intent, Model: a.cfg.Provider.Name(), ModelReason: p.Reason,
		Tool: p.Tool, Target: p.Target(), Args: p.Args, Changes: p.Changes, Request: p.Request(),
	}
	return e
}

// record appends the entry. On failure the user is told, loudly, and the
// trail stays unhealthy so that no further change is proposed.
func (a *Agent) record(e audit.Entry, emit func(Event)) bool {
	if _, err := a.cfg.Audit.Append(e); err != nil {
		emit(EventNotice{Warning: true, Text: fmt.Sprintf(
			"AUDIT FAILURE: could not record %s on %s %s (outcome: %s): %v. This action is NOT in the audit trail. k2stui will propose no further changes until this is fixed and it is restarted.",
			e.Tool, e.Target.Kind, qualified(e.Target.Namespace, e.Target.Name), e.Outcome.Status, err)})
		return false
	}
	return true
}

func qualified(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

func (a *Agent) dryRunRejected(p *tools.Plan, cause error, intent string, edited bool, emit func(Event)) (string, bool) {
	reason := kube.Reason(cause)
	entry := a.entry(p, intent)
	entry.DryRun.Result, entry.DryRun.Message = audit.DryRunRejected, reason
	entry.Decision.Action = audit.DecisionNone
	entry.Outcome.Status = audit.OutcomeDryRunRejected
	entry.Outcome.Detail = "rejected by the API server before approval; nothing was applied"
	audited := a.record(entry, emit)

	what := "The API server rejected this change in a dry-run"
	if kube.IsDenied(cause) {
		what = "Permission denied: the cluster does not allow the current user to make this change (rejected in a dry-run)"
	}
	emit(EventProposalClosed{ID: p.ID, Title: p.Title, Outcome: audit.OutcomeDryRunRejected, Detail: reason, Audited: audited})
	prefix := ""
	if edited {
		prefix = "The user edited the proposal. "
	}
	return fmt.Sprintf("%s%s, so it was not offered to the user for approval and nothing was applied. Server said: %s", prefix, what, reason), true
}

func (a *Agent) apply(ctx context.Context, p *tools.Plan, grant *approval.Grant, entry audit.Entry, emit func(Event)) (string, bool) {
	entry.Decision.Action = audit.DecisionApproved

	// Write-ahead: the approval is on disk before the request is sent, so
	// that no crash, kill or quit can leave a change without a record. If it
	// cannot be written, nothing is sent.
	ahead := entry
	ahead.Outcome.Status = audit.OutcomeApplying
	ahead.Outcome.Detail = "approved; the request is about to be sent. The next entry for this proposal records the result — if there is none, the result is unknown: check the cluster"
	if !a.record(ahead, emit) {
		emit(EventProposalClosed{ID: p.ID, Title: p.Title, Outcome: audit.OutcomeFailed, Detail: "the audit trail could not be written, so the change was not sent"})
		return "not applied: approved by the user, but the audit trail could not be written, and k2stui does not act unrecorded.", true
	}

	// The human said yes: carry the request through even if the turn is
	// cancelled meanwhile, so the outcome is known and recorded rather than
	// left in doubt.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	result, err := p.Apply(actx, grant)

	if err != nil {
		reason := kube.Reason(err)
		var stale *tools.StaleError
		entry.Outcome.Status, entry.Outcome.Error = audit.OutcomeFailed, reason
		audited := a.record(entry, emit)
		emit(EventProposalClosed{ID: p.ID, Title: p.Title, Outcome: audit.OutcomeFailed, Detail: reason, Audited: audited})
		switch {
		case errors.As(err, &stale):
			return reason, true
		case kube.IsDenied(err):
			return "apply failed after approval — permission denied: the cluster does not allow the current user to make this change: " + reason, true
		default:
			return "apply failed after approval (the dry-run had passed): " + reason + ". Nothing else was attempted.", true
		}
	}

	entry.Outcome.Status, entry.Outcome.Detail = audit.OutcomeApplied, result
	audited := a.record(entry, emit)
	emit(EventProposalClosed{ID: p.ID, Title: p.Title, Outcome: audit.OutcomeApplied, Detail: result, Audited: audited})
	if !audited {
		result += " WARNING: the change was applied but could NOT be written to the audit trail; tell the user."
	}
	return "approved by user and " + result, false
}
