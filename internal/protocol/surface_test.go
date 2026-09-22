package protocol

import (
	"testing"

	"github.com/verydemo/newapi-box/relaykit/dto"
	"github.com/verydemo/newapi-box/relaykit/types"
)

func TestMatchInbound(t *testing.T) {
	tests := []struct {
		path      string
		wantFmt   types.RelayFormat
		wantModel string
		wantOK    bool
	}{
		{"/v1/chat/completions", types.RelayFormatOpenAI, "", true},
		{"/v1/responses", types.RelayFormatOpenAIResponses, "", true},
		{"/v1/messages", types.RelayFormatClaude, "", true},
		{"/v1beta/models/gemini-2.0-flash:generateContent", types.RelayFormatGemini, "gemini-2.0-flash", true},
		{"/v1beta/models/gemini-2.0-flash:streamGenerateContent", types.RelayFormatGemini, "gemini-2.0-flash", true},
		{"/v1/embeddings", "", "", false},
		{"/health", "", "", false},
	}

	for _, test := range tests {
		format, model, ok := MatchInbound(test.path)
		if ok != test.wantOK || format != test.wantFmt || model != test.wantModel {
			t.Errorf("MatchInbound(%q) = (%q, %q, %v), want (%q, %q, %v)",
				test.path, format, model, ok, test.wantFmt, test.wantModel, test.wantOK)
		}
	}
}

func TestDecodeRequestPerProtocol(t *testing.T) {
	tests := []struct {
		format types.RelayFormat
		body   string
		check  func(any) bool
	}{
		{
			format: types.RelayFormatOpenAI,
			body:   `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			check: func(v any) bool {
				req, ok := v.(*dto.GeneralOpenAIRequest)
				return ok && req.Model == "gpt-4o" && len(req.Messages) == 1
			},
		},
		{
			format: types.RelayFormatClaude,
			body:   `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			check: func(v any) bool {
				req, ok := v.(*dto.ClaudeRequest)
				return ok && req.Model == "claude-sonnet-4-5" && req.MaxTokens != nil && *req.MaxTokens == 64
			},
		},
		{
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"gpt-5","input":"hi"}`,
			check: func(v any) bool {
				req, ok := v.(*dto.OpenAIResponsesRequest)
				return ok && req.Model == "gpt-5"
			},
		},
		{
			format: types.RelayFormatGemini,
			body:   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			check: func(v any) bool {
				req, ok := v.(*dto.GeminiChatRequest)
				return ok && len(req.Contents) == 1
			},
		},
	}

	for _, test := range tests {
		value, err := DecodeRequest(test.format, []byte(test.body))
		if err != nil {
			t.Errorf("DecodeRequest(%s): %v", test.format, err)
			continue
		}
		if !test.check(value) {
			t.Errorf("DecodeRequest(%s) produced unexpected value %#v", test.format, value)
		}
	}
}

func TestIsStreamRequest(t *testing.T) {
	openAIStream := true
	openAINonStream := false

	if !IsStreamRequest(types.RelayFormatOpenAI, &dto.GeneralOpenAIRequest{Stream: &openAIStream}, "/v1/chat/completions") {
		t.Error("OpenAI body flag should select streaming")
	}
	if IsStreamRequest(types.RelayFormatOpenAI, &dto.GeneralOpenAIRequest{Stream: &openAINonStream}, "/v1/chat/completions") {
		t.Error("OpenAI explicit stream=false should not stream")
	}
	if IsStreamRequest(types.RelayFormatOpenAI, &dto.GeneralOpenAIRequest{}, "/v1/chat/completions") {
		t.Error("OpenAI absent stream flag should not stream")
	}

	// Gemini selects streaming through the URL method, not a body field.
	if !IsStreamRequest(types.RelayFormatGemini, &dto.GeminiChatRequest{}, "/v1beta/models/m:streamGenerateContent") {
		t.Error("Gemini streamGenerateContent should stream")
	}
	if IsStreamRequest(types.RelayFormatGemini, &dto.GeminiChatRequest{}, "/v1beta/models/m:generateContent") {
		t.Error("Gemini generateContent should not stream")
	}
}

func TestSetModelWritesBackForProtocolsWithoutOne(t *testing.T) {
	// Gemini's converted request starts without a model: the name came from
	// the URL, so the proxy must write it back before forwarding.
	gemini := &dto.GeminiChatRequest{}
	SetModel(gemini, "gpt-4o")
	if got := ClientModel(types.RelayFormatGemini, gemini, "gpt-4o"); got != "gpt-4o" {
		t.Fatalf("model = %q", got)
	}

	chat := &dto.GeneralOpenAIRequest{Model: "old"}
	SetModel(chat, "new")
	if chat.Model != "new" {
		t.Fatalf("chat model = %q", chat.Model)
	}

	claude := &dto.ClaudeRequest{Model: "old"}
	SetModel(claude, "new")
	if claude.Model != "new" {
		t.Fatalf("claude model = %q", claude.Model)
	}
}
