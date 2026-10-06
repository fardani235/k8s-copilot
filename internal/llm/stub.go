package llm

import (
	"context"
	"errors"
	"sync"
)

// Stub is a scripted provider: each SendTurn returns the next prepared step.
// It makes the loop, the gate and the audit trail testable without a model.
type Stub struct {
	mu    sync.Mutex
	steps []StubStep
	// Requests records every request received, for assertions.
	Requests []Request
	// Loop makes the last step repeat forever instead of running out.
	Loop bool
}

// StubStep is one scripted reply: a response, an error, or a function of the
// request.
type StubStep struct {
	Response Response
	Err      error
	Func     func(Request) (Response, error)
}

// NewStub returns a provider that plays steps in order.
func NewStub(steps ...StubStep) *Stub { return &Stub{steps: steps} }

func (s *Stub) Name() string { return "stub/scripted" }

func (s *Stub) SendTurn(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, req)
	if len(s.steps) == 0 {
		return Response{}, errors.New("stub provider: script exhausted")
	}
	step := s.steps[0]
	if !(s.Loop && len(s.steps) == 1) {
		s.steps = s.steps[1:]
	}
	if step.Func != nil {
		return step.Func(req)
	}
	return step.Response, step.Err
}

// Calls returns how many turns were sent.
func (s *Stub) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Requests)
}

// Text is a step that ends the turn with a message.
func Text(msg string) StubStep { return StubStep{Response: Response{Text: msg}} }

// Call is a step that requests tools.
func Call(calls ...ToolCall) StubStep { return StubStep{Response: Response{ToolCalls: calls}} }

// Fail is a step that returns a provider error.
func Fail(err error) StubStep { return StubStep{Err: err} }
