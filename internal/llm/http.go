package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config selects and configures a provider. The API key is passed in by the
// caller (who read it from the environment); it is never logged, never put in
// an error message and never written to the audit trail.
type Config struct {
	Provider  string
	Model     string
	BaseURL   string
	APIKey    string
	MaxTokens int
	Timeout   time.Duration
}

// Preset holds a provider's defaults.
type Preset struct {
	BaseURL string
	KeyEnv  string // environment variable holding the API key
	Model   string // default model; empty means the user must choose
	// KeyOptional is true for endpoints that may need no key (local servers).
	KeyOptional bool
}

// Presets are the providers selectable by name. "openai-compatible" covers
// anything else that speaks the chat-completions protocol (Ollama, vLLM,
// LiteLLM, a corporate gateway, …) given a base URL.
var Presets = map[string]Preset{
	"anthropic":         {BaseURL: "https://api.anthropic.com", KeyEnv: "ANTHROPIC_API_KEY", Model: "claude-opus-5-5"},
	"openai":            {BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY"},
	"openrouter":        {BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY"},
	"openai-compatible": {KeyEnv: "K8S_COPILOT_API_KEY", KeyOptional: true},
}

// ProviderNames lists the selectable providers.
func ProviderNames() []string {
	return []string{"anthropic", "openai", "openrouter", "openai-compatible"}
}

// New builds the configured provider. Every problem it reports is something
// the user can fix in configuration.
func New(cfg Config) (Provider, error) {
	preset, ok := Presets[cfg.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown model provider %q (choose one of: %s)", cfg.Provider, strings.Join(ProviderNames(), ", "))
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = preset.BaseURL
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("provider %q needs a base URL: set --base-url or K8S_COPILOT_BASE_URL (for example http://localhost:11434/v1)", cfg.Provider)
	}
	if err := checkBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.Model == "" {
		cfg.Model = preset.Model
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("provider %q has no default model: set --model or K8S_COPILOT_MODEL", cfg.Provider)
	}
	if cfg.APIKey == "" && !preset.KeyOptional {
		return nil, fmt.Errorf("no API key for provider %q", cfg.Provider)
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	// Never follow redirects: Go would resend the body and the x-api-key
	// header to wherever the redirect points, plain http included.
	hc := &http.Client{Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if cfg.Provider == "anthropic" {
		return &anthropic{cfg: cfg, hc: hc}, nil
	}
	return &openAI{cfg: cfg, hc: hc}, nil
}

// checkBaseURL refuses plain http to anything but this machine: requests
// carry the API key and whatever the agent read from the cluster.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid provider base URL %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("refusing provider base URL %q: plain http is only allowed for localhost, because requests carry your API key and cluster data", raw)
	default:
		return fmt.Errorf("invalid provider base URL %q: scheme must be https", raw)
	}
}

// postJSON sends body and decodes a 200 response into out. Transient failures
// (network, 408, 429, 5xx) are retried twice with a short backoff; everything
// else is returned as an *Error whose message comes from extractErr.
func postJSON(ctx context.Context, hc *http.Client, provider, endpoint string, headers map[string]string, body, out any, extractErr func([]byte) string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return &Error{Provider: provider, Msg: "cannot encode request: " + err.Error()}
	}
	backoff := []time.Duration{time.Second, 3 * time.Second}
	for attempt := 0; ; attempt++ {
		perr := postOnce(ctx, hc, provider, endpoint, headers, payload, out, extractErr)
		if perr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !perr.Retryable() || attempt >= len(backoff) {
			return perr
		}
		select {
		case <-time.After(backoff[attempt]):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func postOnce(ctx context.Context, hc *http.Client, provider, endpoint string, headers map[string]string, payload []byte, out any, extractErr func([]byte) string) *Error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return &Error{Provider: provider, Status: 400, Msg: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			if ue.Timeout() {
				return &Error{Provider: provider, Msg: "timed out waiting for the model"}
			}
			err = ue.Err // drop the URL from the message
		}
		return &Error{Provider: provider, Msg: err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &Error{Provider: provider, Msg: "reading response: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		msg := extractErr(data)
		if msg == "" {
			msg = strings.TrimSpace(string(data))
			if len(msg) > 300 {
				msg = msg[:300] + "…"
			}
		}
		switch resp.StatusCode {
		case 401, 403:
			msg += " (check the API key)"
		case 404:
			msg += " (check the model name and base URL)"
		}
		return &Error{Provider: provider, Status: resp.StatusCode, Msg: msg}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Provider: provider, Status: 502, Msg: "unreadable response: " + err.Error()}
	}
	return nil
}
