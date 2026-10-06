package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// conversation is one of everything: a user turn, an assistant turn with a
// tool call, the tool's result.
func conversation() Request {
	return Request{
		System: "be brief",
		Tools:  []Tool{{Name: "get_logs", Description: "read logs", Schema: json.RawMessage(`{"type":"object","properties":{"pod":{"type":"string"}}}`)}},
		Messages: []Message{
			{Role: User, Text: "why is web-1 restarting?"},
			{Role: Assistant, Text: "Let me look.", ToolCalls: []ToolCall{{ID: "t1", Name: "get_logs", Args: json.RawMessage(`{"pod":"web-1"}`)}}},
			{Role: User, ToolResults: []ToolResult{{CallID: "t1", Name: "get_logs", Content: "panic: oom", IsError: false}}},
		},
	}
}

func serve(t *testing.T, status int, reply string, check func(r *http.Request, body map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		if check != nil {
			check(r, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnthropicAdapter(t *testing.T) {
	srv := serve(t, 200, `{"content":[{"type":"text","text":"It is out of memory."},
		{"type":"tool_use","id":"t2","name":"scale","input":{"replicas":3}}],
		"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":50}}`,
		func(r *http.Request, body map[string]any) {
			if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "sk-test" || r.Header.Get("anthropic-version") == "" {
				t.Errorf("request: %s %v", r.URL.Path, r.Header)
			}
			if body["model"] != "claude-opus-5-5" {
				t.Errorf("model: %v", body["model"])
			}
			msgs := body["messages"].([]any)
			if len(msgs) != 3 {
				t.Fatalf("%d messages", len(msgs))
			}
			asst := msgs[1].(map[string]any)["content"].([]any)
			use := asst[1].(map[string]any)
			if use["type"] != "tool_use" || use["id"] != "t1" || use["input"].(map[string]any)["pod"] != "web-1" {
				t.Errorf("tool_use block: %v", use)
			}
			res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
			if res["type"] != "tool_result" || res["tool_use_id"] != "t1" || res["content"] != "panic: oom" {
				t.Errorf("tool_result block: %v", res)
			}
			tool := body["tools"].([]any)[0].(map[string]any)
			if tool["name"] != "get_logs" || tool["input_schema"] == nil {
				t.Errorf("tool: %v", tool)
			}
			if sys := body["system"].([]any)[0].(map[string]any); sys["text"] != "be brief" {
				t.Errorf("system: %v", sys)
			}
		})
	p, err := New(Config{Provider: "anthropic", BaseURL: srv.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "anthropic/claude-opus-5-5" {
		t.Fatalf("name %q", p.Name())
	}
	resp, err := p.SendTurn(context.Background(), conversation())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "It is out of memory." || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "scale" ||
		resp.ToolCalls[0].ID != "t2" || string(resp.ToolCalls[0].Args) != `{"replicas":3}` || resp.Usage.InputTokens != 150 {
		t.Fatalf("response: %+v", resp)
	}
}

// The same conversation through the other protocol: the loop does not change.
func TestOpenAICompatibleAdapter(t *testing.T) {
	srv := serve(t, 200, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
		"tool_calls":[{"id":"call_9","type":"function","function":{"name":"scale","arguments":"{\"replicas\":3}"}},
		              {"id":"call_10","type":"function","function":{"name":"get_logs","arguments":"{not json"}}]}}],
		"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
		func(r *http.Request, body map[string]any) {
			if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-or" {
				t.Errorf("request: %s %v", r.URL.Path, r.Header.Get("Authorization"))
			}
			msgs := body["messages"].([]any)
			roles := ""
			for _, m := range msgs {
				roles += m.(map[string]any)["role"].(string) + " "
			}
			if roles != "system user assistant tool " {
				t.Errorf("roles: %s", roles)
			}
			tc := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
			if tc["id"] != "t1" || tc["function"].(map[string]any)["arguments"] != `{"pod":"web-1"}` {
				t.Errorf("tool call: %v", tc)
			}
			if tr := msgs[3].(map[string]any); tr["tool_call_id"] != "t1" || tr["content"] != "panic: oom" {
				t.Errorf("tool result: %v", tr)
			}
		})
	p, err := New(Config{Provider: "openrouter", BaseURL: srv.URL, APIKey: "sk-or", Model: "some/model"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.SendTurn(context.Background(), conversation())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "call_9" || string(resp.ToolCalls[0].Args) != `{"replicas":3}` {
		t.Fatalf("response: %+v", resp)
	}
	// Broken arguments stay valid JSON (a string) so strict decoding in the
	// registry rejects them instead of anything choking on them.
	if !json.Valid(resp.ToolCalls[1].Args) || resp.ToolCalls[1].Args[0] != '"' {
		t.Fatalf("malformed arguments were passed through raw: %s", resp.ToolCalls[1].Args)
	}
}

func TestProviderErrors(t *testing.T) {
	t.Run("bad key is explained and not retried", func(t *testing.T) {
		n := 0
		srv := serve(t, 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			func(*http.Request, map[string]any) { n++ })
		p, _ := New(Config{Provider: "anthropic", BaseURL: srv.URL, APIKey: "sk-secret-value"})
		_, err := p.SendTurn(context.Background(), conversation())
		var pe *Error
		if !errors.As(err, &pe) || pe.Status != 401 || !strings.Contains(err.Error(), "invalid x-api-key") || !strings.Contains(err.Error(), "check the API key") {
			t.Fatalf("got %v", err)
		}
		if n != 1 {
			t.Fatalf("a 401 was retried (%d requests)", n)
		}
		if strings.Contains(err.Error(), "sk-secret-value") {
			t.Fatal("the error leaks the API key")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		}))
		defer srv.Close()
		p, _ := New(Config{Provider: "anthropic", BaseURL: srv.URL, APIKey: "k", Timeout: 50 * time.Millisecond})
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		_, err := p.SendTurn(ctx, conversation())
		if err == nil {
			t.Fatal("no error on timeout")
		}
	})
	t.Run("cancel stops immediately", func(t *testing.T) {
		srv := serve(t, 500, `{"error":{"message":"boom"}}`, nil)
		p, _ := New(Config{Provider: "anthropic", BaseURL: srv.URL, APIKey: "k"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := p.SendTurn(ctx, conversation()); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

// A redirect must not carry the API key and the conversation somewhere else.
func TestRedirectsAreNotFollowed(t *testing.T) {
	leaked := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = true
		io.WriteString(w, `{"content":[{"type":"text","text":"hi"}]}`)
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	p, _ := New(Config{Provider: "anthropic", BaseURL: srv.URL, APIKey: "sk-secret"})
	_, err := p.SendTurn(context.Background(), conversation())
	if err == nil || leaked {
		t.Fatalf("redirect followed (leaked=%v, err=%v)", leaked, err)
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"unknown provider", Config{Provider: "skynet", APIKey: "k"}, "unknown model provider"},
		{"missing key", Config{Provider: "anthropic"}, "no API key"},
		{"model required", Config{Provider: "openai", APIKey: "k"}, "no default model"},
		{"base url required", Config{Provider: "openai-compatible", Model: "m"}, "needs a base URL"},
		{"plain http to a remote host", Config{Provider: "openai-compatible", Model: "m", BaseURL: "http://llm.internal.example/v1"}, "plain http is only allowed for localhost"},
		{"garbage url", Config{Provider: "openai-compatible", Model: "m", BaseURL: "not a url"}, "invalid provider base URL"},
	}
	for _, c := range cases {
		if _, err := New(c.cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	// A local server needs no key.
	if _, err := New(Config{Provider: "openai-compatible", Model: "llama3", BaseURL: "http://localhost:11434/v1"}); err != nil {
		t.Errorf("local endpoint without key: %v", err)
	}
}

func TestStubIsDeterministic(t *testing.T) {
	s := NewStub(Call(ToolCall{ID: "1", Name: "get_logs"}), Text("done"))
	r1, _ := s.SendTurn(context.Background(), Request{})
	r2, _ := s.SendTurn(context.Background(), Request{})
	if len(r1.ToolCalls) != 1 || r2.Text != "done" || s.Calls() != 2 {
		t.Fatalf("%+v %+v", r1, r2)
	}
	if _, err := s.SendTurn(context.Background(), Request{}); err == nil {
		t.Fatal("an exhausted script must fail, not invent a reply")
	}
}
