package main

// The AI Code Assistant's provider layer, conversation and usage accounting.
//
// Three rules shape this file:
//
//   - No vendor is wired into a handler. Everything goes through AIProvider,
//     and the two implementations below (Anthropic's Messages API and the
//     OpenAI chat-completions shape) cover every endpoint this manager is
//     expected to reach: OpenAI itself, an OpenAI-compatible gateway, Ollama
//     and vLLM, hosted or local, by base URL alone.
//   - Credentials never leave the server. The key is read from the
//     environment or stored in the database, and the configuration handler
//     reports whether one is set — never what it is.
//   - Nothing is invented. A provider that does not report a token count
//     yields null, not a guess, and a figure this manager worked out itself
//     is labelled estimated. Cost is computed only from rates the operator
//     configured, because published prices change and a wrong number here
//     would be worse than none.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// A conversation turn as the provider sees it.
type ChatMessage struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}
type ChatRequest struct {
	Model       string
	System      string
	Messages    []ChatMessage
	MaxTokens   int
	Temperature float64
}

// AIUsage is the normalised accounting for one request. Every count is a
// pointer: a provider that does not report a field leaves it null, and the UI
// shows "not reported" rather than a zero that reads like a measurement.
type AIUsage struct {
	InputTokens       *int `json:"inputTokens"`
	CachedInputTokens *int `json:"cachedInputTokens"`
	OutputTokens      *int `json:"outputTokens"`
	ReasoningTokens   *int `json:"reasoningTokens"`
	// What this manager put in front of the model. Always its own figure, so
	// always estimated; ContextLimit is whatever the operator configured.
	ContextTokens *int `json:"contextTokens"`
	ContextLimit  *int `json:"contextLimit"`
	// True when a count above was worked out here instead of reported by the
	// provider. The UI must say so wherever it shows one.
	Estimated bool `json:"estimated"`
	// Only ever computed from rates in the configuration; null otherwise.
	Cost     *float64 `json:"cost"`
	Currency string   `json:"currency,omitempty"`
	// Why cost is null, so the panel can say something useful.
	CostNote  string `json:"costNote,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
}

// AIEvent is one thing that happened during a generation. The stream carries
// text as it arrives, then exactly one terminal event: usage, or error.
type AIEvent struct {
	Type  string `json:"type"` // text | usage | error
	Text  string `json:"text,omitempty"`
	Usage *AIUsage
	Err   string `json:"error,omitempty"`
}

// AIProvider is the whole vendor surface. A new backend is one implementation
// of this and one line in newProvider; nothing else in the feature changes.
type AIProvider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (<-chan AIEvent, error)
}

// The provider kinds this manager understands. "openai" is the compatible
// chat-completions shape, which is what Ollama, vLLM, RunPod-hosted vLLM and
// the OpenAI-compatible gateways all speak; they differ only by base URL.
var aiProviders = []struct {
	ID, Label, DefaultBase string
	NeedsKey               bool
}{
	{"anthropic", "Anthropic (Claude)", "https://api.anthropic.com", true},
	{"openai", "OpenAI", "https://api.openai.com/v1", true},
	{"openai-compatible", "OpenAI-compatible endpoint", "", true},
	{"ollama", "Ollama", "http://127.0.0.1:11434/v1", false},
	{"vllm", "vLLM", "", false},
}

func providerSpec(id string) (string, bool, bool) {
	for _, p := range aiProviders {
		if p.ID == id {
			return p.DefaultBase, p.NeedsKey, true
		}
	}
	return "", false, false
}

// AISettings is the operator's configuration. The API key is deliberately not
// a field: it is held separately (see aiKey) and never marshalled towards a
// browser.
type AISettings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"baseUrl"`
	// Zero means "not configured": the panel then reports the context window
	// as unknown instead of showing a made-up utilisation.
	ContextLimit int `json:"contextLimit"`
	// Rates per million tokens, in Currency. Zero disables cost reporting for
	// that side rather than reporting a zero cost.
	InputPrice       float64 `json:"inputPrice"`
	CachedInputPrice float64 `json:"cachedInputPrice"`
	OutputPrice      float64 `json:"outputPrice"`
	Currency         string  `json:"currency"`
	// Seconds. A generation that outlives this is cancelled like any other.
	TimeoutSeconds int `json:"timeoutSeconds"`
	MaxTokens      int `json:"maxTokens"`
}

const aiSettingsKey = "ai"
const aiKeyKey = "ai-key"

// Bounds. A request body is already capped at maxRequestBody; these keep a
// single conversation, and a single generation, from growing without limit.
const (
	maxAIPrompt      = 32000
	maxAIMessages    = 60
	defaultAITimeout = 180
	maxAITimeout     = 900
	defaultMaxTokens = 8000
)

func (s *AISettings) normalize() {
	if s.TimeoutSeconds <= 0 {
		s.TimeoutSeconds = defaultAITimeout
	}
	if s.TimeoutSeconds > maxAITimeout {
		s.TimeoutSeconds = maxAITimeout
	}
	if s.MaxTokens <= 0 {
		s.MaxTokens = defaultMaxTokens
	}
	if s.Currency == "" {
		s.Currency = "USD"
	}
	if s.BaseURL == "" {
		s.BaseURL, _, _ = providerSpec(s.Provider)
	}
	s.BaseURL = strings.TrimRight(s.BaseURL, "/")
}

func (s AISettings) validate() error {
	base, _, known := providerSpec(s.Provider)
	if !known {
		return errors.New("unknown provider")
	}
	if strings.TrimSpace(s.Model) == "" {
		return errors.New("a model name is required")
	}
	if len(s.Model) > 200 {
		return errors.New("that model name is too long")
	}
	if base == "" && s.BaseURL == "" {
		return errors.New("this provider needs a base URL")
	}
	if s.BaseURL != "" {
		if !strings.HasPrefix(s.BaseURL, "http://") && !strings.HasPrefix(s.BaseURL, "https://") {
			return errors.New("the base URL must be an http:// or https:// address")
		}
		if len(s.BaseURL) > 500 {
			return errors.New("that base URL is too long")
		}
	}
	if s.ContextLimit < 0 || s.ContextLimit > 100_000_000 {
		return errors.New("that context limit is out of range")
	}
	for _, p := range []float64{s.InputPrice, s.CachedInputPrice, s.OutputPrice} {
		if p < 0 || p > 100000 {
			return errors.New("a price per million tokens is out of range")
		}
	}
	if s.MaxTokens < 0 || s.MaxTokens > 200000 {
		return errors.New("that output token limit is out of range")
	}
	return nil
}

// setting reads one JSON blob from the shared settings table.
func (a *App) setting(key string, v any) error {
	var b string
	if e := a.db.QueryRow("SELECT data FROM app_settings WHERE key=?", key).Scan(&b); e != nil {
		return e
	}
	return json.Unmarshal([]byte(b), v)
}
func (a *App) setSetting(key string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = a.db.Exec("INSERT INTO app_settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data", key, string(b))
	return e
}

func (a *App) aiSettings() AISettings {
	var s AISettings
	a.setting(aiSettingsKey, &s)
	s.normalize()
	return s
}

// aiKey resolves the credential. The environment wins, so a deployment that
// keeps its secrets in the service EnvironmentFile — as this one does — never
// has to put one in the database at all.
func (a *App) aiKey() string {
	if k := strings.TrimSpace(os.Getenv("YDFS_AI_API_KEY")); k != "" {
		return k
	}
	var k string
	a.setting(aiKeyKey, &k)
	return k
}
func (a *App) aiKeyFromEnv() bool {
	return strings.TrimSpace(os.Getenv("YDFS_AI_API_KEY")) != ""
}

// newProvider is the only place a vendor is chosen.
func (a *App) newProvider(s AISettings) (AIProvider, error) {
	_, needsKey, known := providerSpec(s.Provider)
	if !known {
		return nil, errors.New("No AI provider is configured. Choose one in Provider settings before starting a session.")
	}
	key := a.aiKey()
	if needsKey && key == "" {
		return nil, fmt.Errorf("The %s API key is not configured. Set it in Provider settings before starting a session.", s.Provider)
	}
	client := &http.Client{Timeout: time.Duration(s.TimeoutSeconds+30) * time.Second}
	if s.Provider == "anthropic" {
		return &anthropicProvider{base: s.BaseURL, key: key, client: client}, nil
	}
	return &openaiProvider{name: s.Provider, base: s.BaseURL, key: key, client: client}, nil
}

// postJSON is the shared call: a streaming POST whose body the caller reads as
// server-sent events. The credential travels in a header, never in the URL, so
// it cannot reach a log line or a proxy's access record.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body any) (*http.Response, error) {
	b, e := json.Marshal(body)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, e := client.Do(req)
	if e != nil {
		return nil, errors.New(sanitizeAIError(e.Error()))
	}
	if res.StatusCode/100 != 2 {
		defer res.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("the provider rejected the request (%d): %s", res.StatusCode, sanitizeAIError(providerMessage(msg)))
	}
	return res, nil
}

// providerMessage digs the human-readable part out of an error body, falling
// back to the raw text when it is not the shape we expected.
func providerMessage(b []byte) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &v) == nil {
		if v.Error.Message != "" {
			return v.Error.Message
		}
		if v.Message != "" {
			return v.Message
		}
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	if s == "" {
		s = "no detail returned"
	}
	return s
}

// sanitizeAIError keeps a credential out of anything shown or logged. Provider
// errors quote the offending request often enough to be worth the belt.
func sanitizeAIError(s string) string {
	for _, tok := range redactions() {
		if len(tok) >= 8 {
			s = strings.ReplaceAll(s, tok, "[redacted]")
		}
	}
	return s
}

// redactions is every secret this process holds that could be quoted back at
// it by a provider error. Set once at startup so sanitizeAIError stays pure.
var redactions = func() []string { return nil }

// sseLines walks a server-sent-event body, handing each "data:" payload to fn.
// Both providers stream in this shape, so the framing is parsed once.
func sseLines(body io.Reader, fn func(data []byte) bool) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if !fn([]byte(data)) {
			return nil
		}
	}
	return sc.Err()
}

func intp(v int) *int { return &v }

// ---------------------------------------------------------------- Anthropic

type anthropicProvider struct {
	base, key string
	client    *http.Client
}

func (p *anthropicProvider) Name() string { return "anthropic" }
func (p *anthropicProvider) Chat(ctx context.Context, req ChatRequest) (<-chan AIEvent, error) {
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     true,
		"messages":   req.Messages,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	res, e := postJSON(ctx, p.client, p.base+"/v1/messages", map[string]string{
		"x-api-key":         p.key,
		"anthropic-version": "2023-06-01",
	}, body)
	if e != nil {
		return nil, e
	}
	out := make(chan AIEvent, 64)
	go func() {
		defer close(out)
		defer res.Body.Close()
		usage := AIUsage{Provider: "anthropic", Model: req.Model}
		reported := false
		e := sseLines(res.Body, func(data []byte) bool {
			var ev struct {
				Type  string `json:"type"`
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
				Message struct {
					Usage anthropicUsage `json:"usage"`
				} `json:"message"`
				Usage anthropicUsage `json:"usage"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &ev) != nil {
				return true
			}
			switch ev.Type {
			case "content_block_delta":
				if ev.Delta.Text != "" {
					out <- AIEvent{Type: "text", Text: ev.Delta.Text}
				}
			case "message_start":
				ev.Message.Usage.apply(&usage)
				reported = true
			case "message_delta":
				ev.Usage.apply(&usage)
				reported = true
			case "error":
				out <- AIEvent{Type: "error", Err: sanitizeAIError(ev.Error.Message)}
				return false
			}
			return true
		})
		if e != nil && ctx.Err() == nil {
			out <- AIEvent{Type: "error", Err: sanitizeAIError(e.Error())}
			return
		}
		if !reported {
			usage.Estimated = true
		}
		out <- AIEvent{Type: "usage", Usage: &usage}
	}()
	return out, nil
}

type anthropicUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
}

// apply merges what this event carried, leaving every field the provider did
// not mention exactly as it was — the counts arrive across two events.
func (u anthropicUsage) apply(dst *AIUsage) {
	if u.InputTokens != nil {
		dst.InputTokens = u.InputTokens
	}
	if u.OutputTokens != nil {
		dst.OutputTokens = u.OutputTokens
	}
	if u.CacheReadInputTokens != nil {
		dst.CachedInputTokens = u.CacheReadInputTokens
	}
}

// ------------------------------------------------ OpenAI and compatible APIs

type openaiProvider struct {
	name, base, key string
	client          *http.Client
}

func (p *openaiProvider) Name() string { return p.name }
func (p *openaiProvider) Chat(ctx context.Context, req ChatRequest) (<-chan AIEvent, error) {
	msgs := make([]ChatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, ChatMessage{Role: "system", Content: req.System})
	}
	msgs = append(msgs, req.Messages...)
	body := map[string]any{
		"model":    req.Model,
		"stream":   true,
		"messages": msgs,
		// Ollama and vLLM ignore what they do not know; OpenAI needs this to
		// report usage at all when streaming.
		"stream_options": map[string]any{"include_usage": true},
		"max_tokens":     req.MaxTokens,
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	headers := map[string]string{}
	if p.key != "" {
		headers["Authorization"] = "Bearer " + p.key
	}
	res, e := postJSON(ctx, p.client, p.base+"/chat/completions", headers, body)
	if e != nil {
		return nil, e
	}
	out := make(chan AIEvent, 64)
	go func() {
		defer close(out)
		defer res.Body.Close()
		usage := AIUsage{Provider: p.name, Model: req.Model}
		reported := false
		e := sseLines(res.Body, func(data []byte) bool {
			var ev struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens        *int `json:"prompt_tokens"`
					CompletionTokens    *int `json:"completion_tokens"`
					PromptTokensDetails *struct {
						CachedTokens *int `json:"cached_tokens"`
					} `json:"prompt_tokens_details"`
					CompletionTokensDetails *struct {
						ReasoningTokens *int `json:"reasoning_tokens"`
					} `json:"completion_tokens_details"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &ev) != nil {
				return true
			}
			if ev.Error != nil {
				out <- AIEvent{Type: "error", Err: sanitizeAIError(ev.Error.Message)}
				return false
			}
			for _, c := range ev.Choices {
				if c.Delta.Content != "" {
					out <- AIEvent{Type: "text", Text: c.Delta.Content}
				}
			}
			if ev.Usage != nil {
				reported = true
				usage.InputTokens = ev.Usage.PromptTokens
				usage.OutputTokens = ev.Usage.CompletionTokens
				if d := ev.Usage.PromptTokensDetails; d != nil {
					usage.CachedInputTokens = d.CachedTokens
				}
				if d := ev.Usage.CompletionTokensDetails; d != nil {
					usage.ReasoningTokens = d.ReasoningTokens
				}
			}
			return true
		})
		if e != nil && ctx.Err() == nil {
			out <- AIEvent{Type: "error", Err: sanitizeAIError(e.Error())}
			return
		}
		if !reported {
			// Ollama and some gateways stream no usage block at all.
			usage.Estimated = true
		}
		out <- AIEvent{Type: "usage", Usage: &usage}
	}()
	return out, nil
}

// ------------------------------------------------------------------- costing

// price fills in cost from the configured rates. With no rate configured the
// cost stays null and says why: a made-up figure would be worse than none.
func (s AISettings) price(u *AIUsage) {
	if s.InputPrice == 0 && s.OutputPrice == 0 {
		u.CostNote = "no price per million tokens is configured for this model"
		return
	}
	if u.InputTokens == nil && u.OutputTokens == nil {
		u.CostNote = "the provider reported no token counts"
		return
	}
	total := 0.0
	in, cached := 0, 0
	if u.InputTokens != nil {
		in = *u.InputTokens
	}
	if u.CachedInputTokens != nil {
		cached = *u.CachedInputTokens
		if cached > in {
			// Anthropic reports cache reads outside input_tokens; OpenAI
			// reports them inside it. Never let the uncached half go negative.
			cached = in
		}
	}
	rate := s.CachedInputPrice
	if rate == 0 {
		rate = s.InputPrice
	}
	total += float64(in-cached) / 1e6 * s.InputPrice
	total += float64(cached) / 1e6 * rate
	if u.OutputTokens != nil {
		total += float64(*u.OutputTokens) / 1e6 * s.OutputPrice
	}
	u.Cost = &total
	u.Currency = s.Currency
}

// estimateTokens is this manager's own rough count, used for the context it
// assembled and whenever a provider reports nothing. Every value derived from
// it is flagged estimated; it is never presented as a measurement.
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

// ----------------------------------------------------------------- the state

// AIMessage is one turn as the conversation stores it. Patch is set on an
// assistant turn that proposed file changes (see aiworkspace.go).
type AIMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	At      string   `json:"at"`
	Usage   *AIUsage `json:"usage,omitempty"`
	PatchID string   `json:"patchId,omitempty"`
	// Set when the turn was stopped by the operator, so the transcript shows
	// a partial answer as partial.
	Cancelled bool `json:"cancelled,omitempty"`
}

// AISession is one conversation. Kept whole as a row, like Job and Profile.
type AISession struct {
	ID       string      `json:"id"`
	Started  string      `json:"started"`
	User     string      `json:"user"`
	Messages []AIMessage `json:"messages"`
	Totals   AITotals    `json:"totals"`
	// Which build is currently testing this conversation's work, so the page
	// finds it again after a reload (see aiAdoptBuild).
	Build *AIBuild `json:"build"`
}

// AITotals is the session's running account. The counts are sums of what
// providers actually reported; Requests counts every generation either way.
type AITotals struct {
	Requests          int      `json:"requests"`
	ToolCalls         int      `json:"toolCalls"`
	InputTokens       int      `json:"inputTokens"`
	CachedInputTokens int      `json:"cachedInputTokens"`
	OutputTokens      int      `json:"outputTokens"`
	ReasoningTokens   int      `json:"reasoningTokens"`
	Cost              *float64 `json:"cost"`
	Currency          string   `json:"currency,omitempty"`
	LatencyMs         int64    `json:"latencyMs"`
}

func (t *AITotals) add(u *AIUsage) {
	t.Requests++
	t.LatencyMs += u.LatencyMs
	for _, p := range []struct {
		v   *int
		dst *int
	}{{u.InputTokens, &t.InputTokens}, {u.CachedInputTokens, &t.CachedInputTokens}, {u.OutputTokens, &t.OutputTokens}, {u.ReasoningTokens, &t.ReasoningTokens}} {
		if p.v != nil {
			*p.dst += *p.v
		}
	}
	if u.Cost != nil {
		c := *u.Cost
		if t.Cost != nil {
			c += *t.Cost
		}
		t.Cost = &c
		t.Currency = u.Currency
	}
}

func (a *App) aiSession() (*AISession, error) {
	var id string
	a.setting("ai-session", &id)
	if id != "" {
		var b string
		if e := a.db.QueryRow("SELECT data FROM ai_sessions WHERE id=?", id).Scan(&b); e == nil {
			var s AISession
			if json.Unmarshal([]byte(b), &s) == nil {
				if s.Messages == nil {
					s.Messages = []AIMessage{}
				}
				return &s, nil
			}
		}
	}
	return a.newAISession("")
}

func (a *App) newAISession(user string) (*AISession, error) {
	s := &AISession{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Started: now(), User: user, Messages: []AIMessage{}}
	if e := a.saveAISession(s); e != nil {
		return nil, e
	}
	return s, a.setSetting("ai-session", s.ID)
}

func (a *App) saveAISession(s *AISession) error {
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	_, e = a.db.Exec("INSERT INTO ai_sessions VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", s.ID, string(b))
	return e
}

// recordUsage writes one row per request. The dimensions a later report would
// group by — time, user, provider, model — are columns rather than JSON, so
// aggregating by day, week, month, user, project, provider or model is a query
// and not a migration.
func (a *App) recordUsage(session, user string, u *AIUsage) {
	b, _ := json.Marshal(u)
	a.db.Exec("INSERT INTO ai_usage(at, session, user, project, provider, model, data) VALUES(?,?,?,?,?,?,?)",
		now(), session, user, a.repo, u.Provider, u.Model, string(b))
}

// ---------------------------------------------------------------- the handler

// aiState is everything the page needs to render itself in one call.
type aiState struct {
	Providers []aiProviderInfo `json:"providers"`
	Settings  AISettings       `json:"settings"`
	// Whether a credential is resolvable, and from where. Never the key.
	KeyConfigured bool       `json:"keyConfigured"`
	KeyFromEnv    bool       `json:"keyFromEnv"`
	Ready         bool       `json:"ready"`
	Message       string     `json:"message"`
	Session       *AISession `json:"session"`
	Generating    bool       `json:"generating"`
	Patch         *Patch     `json:"patch"`
	Build         *AIBuild   `json:"build"`
	QuickActions  []aiAction `json:"quickActions"`
	Targets       []string   `json:"targets"`
}
type aiProviderInfo struct {
	ID, Label, DefaultBase string
	NeedsKey               bool
}
type aiAction struct {
	ID, Label, Prompt string
}

// The quick actions above the composer. Each only ever *prepares* an
// instruction in the textarea; nothing is sent and no file is touched until
// the operator presses Send.
var aiActions = []aiAction{
	{"bug", "Fix a bug", "There is a bug in the selected files: describe what goes wrong here.\n\nInvestigate the cause and propose a fix."},
	{"improve", "Improve code", "Improve the selected code. Keep the behaviour identical and explain what you changed and why."},
	{"refactor", "Refactor", "Refactor the selected code for clarity. Do not change behaviour, and keep the change as small as it can be."},
	{"explain", "Explain code", "Explain what the selected code does, step by step. Do not propose any change."},
	{"errors", "Fix compilation errors", "The build failed with the errors below. Work out the cause and propose a fix."},
}

func (a *App) aiInfo(w http.ResponseWriter, r *http.Request) {
	s := a.aiSettings()
	_, needsKey, known := providerSpec(s.Provider)
	key := a.aiKey()
	st := aiState{Settings: s, KeyConfigured: key != "", KeyFromEnv: a.aiKeyFromEnv(), QuickActions: aiActions, Targets: targets}
	for _, p := range aiProviders {
		st.Providers = append(st.Providers, aiProviderInfo{p.ID, p.Label, p.DefaultBase, p.NeedsKey})
	}
	switch {
	case !known:
		st.Message = "No AI provider is configured. Choose one in Provider settings before starting a session."
	case s.Model == "":
		st.Message = "No model is configured. Set one in Provider settings before starting a session."
	case needsKey && key == "":
		st.Message = "The API key for " + s.Provider + " is not configured. Set it in Provider settings before starting a session."
	default:
		st.Ready = true
	}
	sess, e := a.aiSession()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	st.Session = sess
	st.Build = sess.Build
	a.aiMu.Lock()
	st.Generating = a.aiCancel != nil
	st.Patch = a.patch
	a.aiMu.Unlock()
	respond(w, st)
}

func (a *App) aiConfigure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AISettings
		// Empty means "leave the stored key alone"; "-" clears it. The current
		// key is never sent to the browser, so it cannot be echoed back.
		APIKey string `json:"apiKey"`
	}
	if !decode(w, r, &body) {
		return
	}
	s := body.AISettings
	s.normalize()
	if e := s.validate(); e != nil {
		fail(w, 400, e.Error())
		return
	}
	if e := a.setSetting(aiSettingsKey, s); e != nil {
		fail(w, 500, e.Error())
		return
	}
	if k := strings.TrimSpace(body.APIKey); k != "" {
		if k == "-" {
			a.setSetting(aiKeyKey, "")
		} else if len(k) > 500 {
			fail(w, 400, "that API key is too long")
			return
		} else {
			a.setSetting(aiKeyKey, k)
		}
	}
	a.aiInfo(w, r)
}

// aiNewSession starts a fresh conversation. The old one stays in the table:
// its usage rows are the session history a later report aggregates.
func (a *App) aiNewSession(w http.ResponseWriter, r *http.Request) {
	a.aiMu.Lock()
	generating := a.aiCancel != nil
	a.aiMu.Unlock()
	if generating {
		fail(w, 409, "a reply is still being generated; stop it first")
		return
	}
	if _, e := a.newAISession(requestUser(a, r)); e != nil {
		fail(w, 500, e.Error())
		return
	}
	a.aiInfo(w, r)
}

func requestUser(a *App, r *http.Request) string {
	if a.dev {
		return "local-developer"
	}
	return r.Header.Get("X-Forwarded-User")
}

func (a *App) aiCancelGeneration(w http.ResponseWriter, r *http.Request) {
	a.aiMu.Lock()
	cancel := a.aiCancel
	a.aiMu.Unlock()
	if cancel == nil {
		fail(w, 409, "nothing is being generated")
		return
	}
	cancel()
	respond(w, map[string]bool{"ok": true})
}

// aiMessage is the streaming endpoint: it takes the operator's instruction and
// the context they selected, calls the provider, and streams the reply back as
// server-sent events — the same shape the build log already uses, so the UI has
// one streaming idiom.
//
// It is a POST rather than an EventSource GET on purpose: EventSource cannot
// set headers, and every mutating request here has to carry the Origin and
// X-Requested-With that handler() checks.
func (a *App) aiMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
		// Repository-relative paths the operator ticked. Validated against the
		// workspace, never trusted as filesystem paths.
		Files []string `json:"files"`
		// Include the working-tree diff, the failed build's errors, or both.
		IncludeDiff bool   `json:"includeDiff"`
		BuildID     string `json:"buildId"`
		Search      string `json:"search"`
	}
	if !decode(w, r, &body) {
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		fail(w, 400, "type an instruction before sending")
		return
	}
	if len(prompt) > maxAIPrompt {
		fail(w, 400, "that instruction is too long")
		return
	}
	settings := a.aiSettings()
	provider, e := a.newProvider(settings)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	flush, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unavailable")
		return
	}
	// One generation at a time, like one build at a time: the conversation is
	// shared, and two replies interleaving into it would corrupt the history.
	a.aiMu.Lock()
	if a.aiCancel != nil {
		a.aiMu.Unlock()
		fail(w, 409, "a reply is already being generated")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(settings.TimeoutSeconds)*time.Second)
	a.aiCancel = cancel
	a.aiMu.Unlock()
	defer func() {
		cancel()
		a.aiMu.Lock()
		a.aiCancel = nil
		a.aiMu.Unlock()
	}()

	sess, e := a.aiSession()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	ctxBlock, ctxNote, e := a.buildContext(ctx, body.Files, body.Search, body.BuildID, body.IncludeDiff)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	user := prompt
	if ctxBlock != "" {
		user = ctxBlock + "\n\n---\n\n" + prompt
	}
	msgs := make([]ChatMessage, 0, len(sess.Messages)+1)
	for _, m := range sess.Messages {
		msgs = append(msgs, ChatMessage{Role: m.Role, Content: m.Content})
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: user})
	if len(msgs) > maxAIMessages {
		msgs = msgs[len(msgs)-maxAIMessages:]
	}
	contextTokens := estimateTokens(aiSystemPrompt)
	for _, m := range msgs {
		contextTokens += estimateTokens(m.Content)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event string, v any) bool {
		b, _ := json.Marshal(v)
		if _, e := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); e != nil {
			return false
		}
		flush.Flush()
		return true
	}
	fmt.Fprint(w, ": connected\n\n")
	flush.Flush()
	send("context", map[string]any{"tokens": contextTokens, "estimated": true, "note": ctxNote})

	started := time.Now()
	events, e := provider.Chat(ctx, ChatRequest{Model: settings.Model, System: aiSystemPrompt, Messages: msgs, MaxTokens: settings.MaxTokens})
	if e != nil {
		send("failed", map[string]string{"error": e.Error()})
		return
	}
	var reply strings.Builder
	var usage *AIUsage
	var failure string
	for ev := range events {
		switch ev.Type {
		case "text":
			reply.WriteString(ev.Text)
			if !send("text", ev.Text) {
				cancel()
			}
		case "error":
			failure = ev.Err
		case "usage":
			usage = ev.Usage
		}
	}
	cancelled := ctx.Err() != nil
	if usage == nil {
		usage = &AIUsage{Provider: provider.Name(), Model: settings.Model, Estimated: true}
	}
	usage.LatencyMs = time.Since(started).Milliseconds()
	usage.ContextTokens = intp(contextTokens)
	if usage.InputTokens == nil {
		usage.InputTokens = intp(contextTokens)
		usage.Estimated = true
	}
	if usage.OutputTokens == nil {
		usage.OutputTokens = intp(estimateTokens(reply.String()))
		usage.Estimated = true
	}
	if settings.ContextLimit > 0 {
		usage.ContextLimit = intp(settings.ContextLimit)
	}
	settings.price(usage)

	// The turn is recorded even when it was stopped or failed part way: a
	// partial answer is still what the model said, and it was still paid for.
	text := reply.String()
	if strings.TrimSpace(text) != "" || failure == "" {
		patchID := ""
		if p, e := a.stagePatch(ctx, text); e == nil && p != nil {
			patchID = p.ID
		} else if e != nil {
			send("patchError", map[string]string{"error": e.Error()})
		}
		sess.Messages = append(sess.Messages,
			AIMessage{Role: "user", Content: user, At: now()},
			AIMessage{Role: "assistant", Content: text, At: now(), Usage: usage, PatchID: patchID, Cancelled: cancelled})
		sess.Totals.add(usage)
		a.saveAISession(sess)
		a.recordUsage(sess.ID, requestUser(a, r), usage)
	}
	switch {
	case failure != "":
		send("failed", map[string]string{"error": failure})
	case cancelled:
		send("cancelled", map[string]string{"error": "Generation stopped."})
	}
	send("usage", usage)
	send("done", map[string]bool{"ok": true})
}

// aiUsage reports the session account plus the rows behind it, ready to be
// grouped by any of the dimensions recordUsage stores as columns.
func (a *App) aiUsage(w http.ResponseWriter, r *http.Request) {
	sess, e := a.aiSession()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	rows, e := a.db.Query("SELECT at, provider, model, data FROM ai_usage WHERE session=? ORDER BY id DESC LIMIT 200", sess.ID)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var at, provider, model, data string
		if e := rows.Scan(&at, &provider, &model, &data); e != nil {
			fail(w, 500, e.Error())
			return
		}
		var u AIUsage
		json.Unmarshal([]byte(data), &u)
		list = append(list, map[string]any{"at": at, "provider": provider, "model": model, "usage": u})
	}
	respond(w, map[string]any{"session": sess.ID, "totals": sess.Totals, "requests": list})
}
