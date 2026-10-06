package llm

import (
	"context"
	"encoding/json"
	"net/http"
)

// anthropic speaks the Anthropic Messages API directly over net/http.
type anthropic struct {
	cfg Config
	hc  *http.Client
}

func (a *anthropic) Name() string { return "anthropic/" + a.cfg.Model }

type antBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	CacheControl *antCache `json:"cache_control,omitempty"`
}

type antCache struct {
	Type string `json:"type"`
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type antRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    []antBlock   `json:"system,omitempty"`
	Tools     []antTool    `json:"tools,omitempty"`
	Messages  []antMessage `json:"messages"`
}

type antResponse struct {
	Content    []antBlock `json:"content"`
	StopReason string     `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		CacheRead    int `json:"cache_read_input_tokens"`
		CacheCreate  int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (a *anthropic) SendTurn(ctx context.Context, req Request) (Response, error) {
	body := antRequest{Model: a.cfg.Model, MaxTokens: a.cfg.MaxTokens}
	if req.System != "" {
		// The system prompt and tool list are identical on every call of a
		// session: mark the prefix cacheable.
		body.System = []antBlock{{Type: "text", Text: req.System, CacheControl: &antCache{Type: "ephemeral"}}}
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, antTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	for _, m := range req.Messages {
		am := antMessage{Role: string(m.Role)}
		if m.Text != "" {
			am.Content = append(am.Content, antBlock{Type: "text", Text: m.Text})
		}
		for _, c := range m.ToolCalls {
			in := c.Args
			if !json.Valid(in) {
				in = json.RawMessage("{}")
			}
			am.Content = append(am.Content, antBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: in})
		}
		for _, r := range m.ToolResults {
			content := r.Content
			if content == "" {
				content = "(empty)"
			}
			am.Content = append(am.Content, antBlock{Type: "tool_result", ToolUseID: r.CallID, Content: content, IsError: r.IsError})
		}
		if len(am.Content) == 0 {
			continue
		}
		body.Messages = append(body.Messages, am)
	}

	var out antResponse
	err := postJSON(ctx, a.hc, "anthropic", a.cfg.BaseURL+"/v1/messages", map[string]string{
		"x-api-key":         a.cfg.APIKey,
		"anthropic-version": "2023-06-01",
	}, body, &out, func(b []byte) string {
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

	resp := Response{
		Truncated: out.StopReason == "max_tokens",
		Usage: Usage{
			InputTokens:  out.Usage.InputTokens + out.Usage.CacheRead + out.Usage.CacheCreate,
			OutputTokens: out.Usage.OutputTokens,
		},
	}
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			resp.Text += b.Text
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: args})
		}
	}
	return resp, nil
}
