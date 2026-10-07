// Package llm is the boundary between k8s-copilot's agent loop and whichever model
// provider is configured.
//
// The loop is owned by k8s-copilot (internal/agent). A provider does exactly one
// thing: take the conversation so far and return the model's next message —
// text, tool calls, or both. It never executes a tool and never loops. That
// is what lets the loop stop between "the model asked for a change" and "the
// change happens".
package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Role of a message.
type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// ToolCall is the model asking for a tool to be run.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// ToolResult answers one ToolCall.
type ToolResult struct {
	CallID  string
	Name    string
	Content string
	IsError bool
}

// Message is one turn of the conversation.
//
//   - A user message has Text, or ToolResults (answers to the previous
//     assistant message's ToolCalls), never both.
//   - An assistant message has Text and/or ToolCalls.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// Tool describes a callable tool to the model.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Request is one model call.
type Request struct {
	System   string
	Messages []Message
	Tools    []Tool
}

// Response is the model's next message.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	// Truncated is set when the provider cut the reply off at its output
	// limit.
	Truncated bool
	Usage     Usage
}

// Usage is token accounting, when the provider reports it.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Provider sends one turn to a model.
type Provider interface {
	// Name identifies the provider and model, e.g. "anthropic/claude-opus-5-5".
	Name() string
	SendTurn(ctx context.Context, req Request) (Response, error)
}

// Error is a provider failure with enough context to act on.
type Error struct {
	Provider string
	Status   int // HTTP status, 0 for transport errors
	Msg      string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s returned HTTP %d: %s", e.Provider, e.Status, e.Msg)
	}
	return fmt.Sprintf("%s request failed: %s", e.Provider, e.Msg)
}

// Retryable reports whether trying again could help.
func (e *Error) Retryable() bool {
	return e.Status == 0 || e.Status == 408 || e.Status == 429 || e.Status >= 500
}
