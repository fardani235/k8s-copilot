package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// openAI speaks the chat-completions protocol, which OpenAI, OpenRouter and
// most self-hosted gateways (Ollama, vLLM, LiteLLM, …) share.
type openAI struct {
	cfg Config
	hc  *http.Client
}

func (o *openAI) Name() string { return o.cfg.Provider + "/" + o.cfg.Model }

type oaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    *string       `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaiRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Tools    []oaiTool    `json:"tools,omitempty"`
	// OpenAI itself wants max_completion_tokens; the compatible servers
	// still expect max_tokens.
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	MaxTokens           int `json:"max_tokens,omitempty"`
}

type oaiResponse struct {
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func strptr(s string) *string { return &s }

func (o *openAI) SendTurn(ctx context.Context, req Request) (Response, error) {
	body := oaiRequest{Model: o.cfg.Model}
	if o.cfg.Provider == "openai" {
		body.MaxCompletionTokens = o.cfg.MaxTokens
	} else {
		body.MaxTokens = o.cfg.MaxTokens
	}
	if req.System != "" {
		body.Messages = append(body.Messages, oaiMessage{Role: "system", Content: strptr(req.System)})
	}
	for _, m := range req.Messages {
		switch {
		case len(m.ToolResults) > 0:
			for _, r := range m.ToolResults {
				content := r.Content
				if r.IsError {
					content = "ERROR: " + content
				}
				body.Messages = append(body.Messages, oaiMessage{Role: "tool", ToolCallID: r.CallID, Content: strptr(content)})
			}
		case m.Role == Assistant:
			om := oaiMessage{Role: "assistant"}
			if m.Text != "" {
				om.Content = strptr(m.Text)
			}
			for _, c := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, oaiToolCall{ID: c.ID, Type: "function",
					Function: oaiFunction{Name: c.Name, Arguments: string(c.Args)}})
			}
			body.Messages = append(body.Messages, om)
		default:
			body.Messages = append(body.Messages, oaiMessage{Role: "user", Content: strptr(m.Text)})
		}
	}
	for _, t := range req.Tools {
		ot := oaiTool{Type: "function"}
		ot.Function.Name, ot.Function.Description, ot.Function.Parameters = t.Name, t.Description, t.Schema
		body.Tools = append(body.Tools, ot)
	}

	headers := map[string]string{}
	if o.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.cfg.APIKey
	}
	if o.cfg.Provider == "openrouter" {
		headers["X-Title"] = "k8s-copilot"
	}

	var out oaiResponse
	err := postJSON(ctx, o.hc, o.cfg.Provider, o.cfg.BaseURL+"/chat/completions", headers, body, &out, func(b []byte) string {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return e.Error.Message
	})
	if err != nil {
		return Response{}, err
	}
	if len(out.Choices) == 0 {
		return Response{}, &Error{Provider: o.cfg.Provider, Status: 502, Msg: "the response contained no choices"}
	}

	ch := out.Choices[0]
	resp := Response{
		Truncated: ch.FinishReason == "length",
		Usage:     Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens},
	}
	if ch.Message.Content != nil {
		resp.Text = *ch.Message.Content
	}
	for i, c := range ch.Message.ToolCalls {
		args := json.RawMessage(strings.TrimSpace(c.Function.Arguments))
		switch {
		case len(args) == 0:
			args = json.RawMessage("{}")
		case !json.Valid(args):
			// Keep it valid JSON (a string) so it can be stored and replayed;
			// the registry's strict decoding will reject it with a clear error.
			args, _ = json.Marshal(string(args))
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: id, Name: c.Function.Name, Args: args})
	}
	return resp, nil
}
