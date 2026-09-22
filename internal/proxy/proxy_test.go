package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhangjl/newapi-box/relaykit/types"

	"github.com/zhangjl/newapi-box/internal/config"
)

// openAIChatCompletionBody is a minimal non-streaming OpenAI Chat reply.
const openAIChatCompletionBody = `{
  "id": "chatcmpl-1",
  "object": "chat.completion",
  "created": 1,
  "model": "gpt-4o",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
}`

// openAIChatStreamBody is a minimal streaming OpenAI Chat reply.
const openAIChatStreamBody = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

data: [DONE]

`

const claudeMessageBody = `{
  "id": "msg_1",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5",
  "content": [{"type": "text", "text": "hello"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 3, "output_tokens": 2}
}`

const claudeStreamBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

const geminiBody = `{
  "candidates": [{"content": {"role": "model", "parts": [{"text": "hello"}]}, "finishReason": "STOP"}],
  "usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 2, "totalTokenCount": 5}
}`

// upstreamRecorder captures what the converter sent upstream.
type upstreamRecorder struct {
	path string
	body map[string]any
}

// newTestProxy starts a fake upstream and returns a proxy pointed at it.
func newTestProxy(t *testing.T, format types.RelayFormat, reply string, streamReply bool, seen *upstreamRecorder) *Proxy {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if seen != nil {
			seen.path = r.URL.Path
			seen.body = map[string]any{}
			_ = json.Unmarshal(raw, &seen.body)
		}

		// Gemini's streaming endpoint is a different URL method, so the
		// recorder must still serve both shapes.
		if strings.Contains(r.URL.Path, ":streamGenerateContent") || (streamReply && strings.HasPrefix(reply, "data: ")) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, reply)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(upstream.Close)

	handler, err := newTestProxyFrom(t, &config.Config{
		Listen:                 ":0",
		Active:                 "default",
		ClaudeDefaultMaxTokens: 1024,
		Upstreams: []config.Upstream{
			{Name: "default", Format: format, BaseURL: upstream.URL, APIKey: "test-key"},
		},
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return handler
}

// newTestProxyFrom builds a proxy over a throwaway store, so tests exercise the
// same construction path main uses.
func newTestProxyFrom(t *testing.T, cfg *config.Config) (*Proxy, error) {
	t.Helper()
	return New(config.NewStore(filepath.Join(t.TempDir(), "config.json"), cfg))
}

func post(t *testing.T, handler *Proxy, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	return value
}

// TestClaudeClientToOpenAIUpstream covers the most common deployment: an
// Anthropic SDK client reaching an OpenAI-compatible upstream.
func TestClaudeClientToOpenAIUpstream(t *testing.T) {
	seen := &upstreamRecorder{}
	handler := newTestProxy(t, types.RelayFormatOpenAI, openAIChatCompletionBody, false, seen)

	recorder := post(t, handler, "/v1/messages",
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if seen.path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", seen.path)
	}
	if seen.body["model"] != "gpt-4o" {
		t.Fatalf("upstream model = %v", seen.body["model"])
	}

	response := decodeJSON(t, recorder.Body.String())
	if response["type"] != "message" {
		t.Fatalf("expected a Claude message, got %v", response["type"])
	}
	content, ok := response["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %#v", response["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["text"] != "hello" {
		t.Fatalf("content text = %v", block["text"])
	}
}

func TestClaudeClientToOpenAIUpstreamStream(t *testing.T) {
	handler := newTestProxy(t, types.RelayFormatOpenAI, openAIChatStreamBody, true, nil)

	recorder := post(t, handler, "/v1/messages",
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %q\n%s", want, body)
		}
	}
	// The OpenAI sentinel must not leak into a Claude stream.
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Claude stream leaked the OpenAI [DONE] sentinel\n%s", body)
	}
}

// TestOpenAIClientToClaudeUpstream is the reverse direction, and exercises the
// max_tokens injection the Claude API requires.
func TestOpenAIClientToClaudeUpstream(t *testing.T) {
	seen := &upstreamRecorder{}
	handler := newTestProxy(t, types.RelayFormatClaude, claudeMessageBody, false, seen)

	recorder := post(t, handler, "/v1/chat/completions",
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if seen.path != "/v1/messages" {
		t.Fatalf("upstream path = %q", seen.path)
	}
	if _, present := seen.body["max_tokens"]; !present {
		t.Fatalf("converted Claude request is missing max_tokens: %v", seen.body)
	}

	response := decodeJSON(t, recorder.Body.String())
	choices, ok := response["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices = %#v", response["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("message content = %v", message["content"])
	}
}

func TestOpenAIClientToClaudeUpstreamStream(t *testing.T) {
	handler := newTestProxy(t, types.RelayFormatClaude, claudeStreamBody, true, nil)

	recorder := post(t, handler, "/v1/chat/completions",
		`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "hello") {
		t.Errorf("stream lost the assistant text\n%s", body)
	}
	// OpenAI clients wait for the sentinel.
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("OpenAI stream is missing the [DONE] sentinel\n%s", body)
	}
}

// TestSameProtocolPassthrough pins the byte-exact path used when the client
// already speaks the upstream protocol.
func TestSameProtocolPassthrough(t *testing.T) {
	handler := newTestProxy(t, types.RelayFormatOpenAI, openAIChatCompletionBody, false, nil)

	recorder := post(t, handler, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"vendor_extension":{"keep":"me"}}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != strings.TrimSpace(openAIChatCompletionBody) {
		t.Fatalf("passthrough body changed:\n%s", got)
	}
}

func TestGeminiClientToOpenAIUpstream(t *testing.T) {
	seen := &upstreamRecorder{}
	handler := newTestProxy(t, types.RelayFormatOpenAI, openAIChatCompletionBody, false, seen)

	recorder := post(t, handler, "/v1beta/models/gpt-4o:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	// The model name only existed in the URL, so the converted request must
	// have picked it up.
	if seen.body["model"] != "gpt-4o" {
		t.Fatalf("upstream model = %v (body %v)", seen.body["model"], seen.body)
	}

	response := decodeJSON(t, recorder.Body.String())
	if _, present := response["candidates"]; !present {
		t.Fatalf("expected a Gemini response, got %s", recorder.Body.String())
	}
}

func TestOpenAIClientToGeminiUpstream(t *testing.T) {
	seen := &upstreamRecorder{}
	handler := newTestProxy(t, types.RelayFormatGemini, geminiBody, false, seen)

	recorder := post(t, handler, "/v1/chat/completions",
		`{"model":"gemini-2.0-flash","messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if want := "/v1beta/models/gemini-2.0-flash:generateContent"; seen.path != want {
		t.Fatalf("upstream path = %q, want %q", seen.path, want)
	}

	response := decodeJSON(t, recorder.Body.String())
	if _, present := response["choices"]; !present {
		t.Fatalf("expected an OpenAI response, got %s", recorder.Body.String())
	}
}

func TestModelMapRewritesUpstreamModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openAIChatCompletionBody)
	}))
	t.Cleanup(upstream.Close)

	handler, err := newTestProxyFrom(t, &config.Config{
		ClaudeDefaultMaxTokens: 1024,
		Active:                 "default",
		Upstreams: []config.Upstream{
			{Name: "default", Format: types.RelayFormatOpenAI, BaseURL: upstream.URL, APIKey: "k",
				ModelMap: map[string]string{"fast": "gpt-4o-mini"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var captured map[string]any
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openAIChatCompletionBody)
	})

	recorder := post(t, handler, "/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if captured["model"] != "gpt-4o-mini" {
		t.Fatalf("upstream model = %v", captured["model"])
	}
}

func TestErrorShapesMatchCallerProtocol(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	t.Cleanup(upstream.Close)

	handler, err := newTestProxyFrom(t, &config.Config{
		Active: "default",
		Upstreams: []config.Upstream{
			{Name: "default", Format: types.RelayFormatOpenAI, BaseURL: upstream.URL, APIKey: "k"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The upstream status must survive so clients can distinguish retryable
	// failures from their own mistakes.
	recorder := post(t, handler, "/v1/messages",
		`{"model":"gpt-4o","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	handler, err := newTestProxyFrom(t, &config.Config{
		Active: "default",
		Upstreams: []config.Upstream{
			{Name: "default", Format: types.RelayFormatOpenAI, BaseURL: "http://127.0.0.1:1", APIKey: "k"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	recorder := post(t, handler, "/v1/embeddings", `{}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	getRecorder := httptest.NewRecorder()
	handler.ServeHTTP(getRecorder, request)
	if getRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", getRecorder.Code)
	}
}

func TestAdminKeyIsEnforced(t *testing.T) {
	handler, err := newTestProxyFrom(t, &config.Config{
		AdminKey: "secret",
		Active:   "default",
		Upstreams: []config.Upstream{
			{Name: "default", Format: types.RelayFormatOpenAI, BaseURL: "http://127.0.0.1:1", APIKey: "k"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}

	// The client-facing key must be accepted in either protocol's header.
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	request.Header.Set("x-api-key", "secret")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusUnauthorized {
		t.Fatal("x-api-key should authenticate")
	}
}

func TestUpstreamAuthHeadersPerProtocol(t *testing.T) {
	tests := []struct {
		format types.RelayFormat
		check  func(*http.Header) bool
	}{
		{
			format: types.RelayFormatClaude,
			check: func(h *http.Header) bool {
				return h.Get("x-api-key") == "test-key" && h.Get("anthropic-version") != ""
			},
		},
		{
			format: types.RelayFormatOpenAI,
			check: func(h *http.Header) bool {
				return h.Get("Authorization") == "Bearer test-key"
			},
		},
		{
			format: types.RelayFormatGemini,
			check: func(h *http.Header) bool {
				return h.Get("x-goog-api-key") == "test-key"
			},
		},
	}

	for _, test := range tests {
		var header http.Header
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, openAIChatCompletionBody)
		}))

		handler, err := newTestProxyFrom(t, &config.Config{
			ClaudeDefaultMaxTokens: 1024,
			Active:                 "default",
			Upstreams: []config.Upstream{
				{Name: "default", Format: test.format, BaseURL: upstream.URL, APIKey: "test-key"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		path := "/v1/chat/completions"
		if test.format == types.RelayFormatGemini {
			path = "/v1beta/models/gpt-4o:generateContent"
		}
		recorder := post(t, handler, path, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d body = %s", test.format, recorder.Code, recorder.Body.String())
		}
		if !test.check(&header) {
			t.Fatalf("%s: unexpected auth headers %v", test.format, header)
		}
		upstream.Close()
	}
}

func TestConfigPathTemplates(t *testing.T) {
	cfg := config.Upstream{Format: types.RelayFormatGemini}
	if got := cfg.PathFor(types.RelayFormatGemini); got != "/v1beta/models/{{model}}:generateContent" {
		t.Fatalf("gemini path = %q", got)
	}
	if got := cfg.GeminiStreamPathFor(); !strings.Contains(got, "streamGenerateContent") {
		t.Fatalf("gemini stream path = %q", got)
	}
	if got := cfg.PathFor(types.RelayFormatClaude); got != "/v1/messages" {
		t.Fatalf("claude path = %q", got)
	}
}

func TestProxyRejectsUnsupportedPath(t *testing.T) {
	handler, err := newTestProxyFrom(t, &config.Config{
		Active: "default",
		Upstreams: []config.Upstream{
			{Name: "default", Format: types.RelayFormatOpenAI, BaseURL: "http://127.0.0.1:1", APIKey: "k"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Guard against a refactor that broadens inbound routing too far.
	for _, path := range []string{"/v1/audio/speech", "/v1/embeddings", "/v1/rerank"} {
		recorder := post(t, handler, path, `{}`)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, recorder.Code)
		}
	}
}
