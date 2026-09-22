package protocol

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zhangjl/newapi-box/relaykit/dto"
	"github.com/zhangjl/newapi-box/relaykit/types"
)

// inboundRoute maps a client-facing HTTP path prefix to the protocol it
// speaks. Gemini is handled separately because its model name is part of the
// path rather than the body.
type inboundRoute struct {
	prefix string
	format types.RelayFormat
}

var inboundRoutes = []inboundRoute{
	{prefix: "/v1/chat/completions", format: types.RelayFormatOpenAI},
	{prefix: "/v1/responses", format: types.RelayFormatOpenAIResponses},
	{prefix: "/v1/messages", format: types.RelayFormatClaude},
}

const geminiPathPrefix = "/v1beta/models/"

// MatchInbound resolves the client protocol from a request path. The second
// return is the model name extracted from the path, which only Gemini uses.
func MatchInbound(path string) (types.RelayFormat, string, bool) {
	for _, route := range inboundRoutes {
		if path == route.prefix || strings.HasPrefix(path, route.prefix+"/") {
			return route.format, "", true
		}
	}
	if model, ok := geminiModelFromPath(path); ok {
		return types.RelayFormatGemini, model, true
	}
	return "", "", false
}

// geminiModelFromPath extracts the model from
// /v1beta/models/{model}:generateContent and its streamGenerateContent variant.
func geminiModelFromPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, geminiPathPrefix)
	if !ok {
		return "", false
	}
	model, method, found := strings.Cut(rest, ":")
	if !found || model == "" {
		return "", false
	}
	switch method {
	case "generateContent", "streamGenerateContent":
		return model, true
	default:
		return "", false
	}
}

// IsStreamRequest reports whether the decoded request asked for streaming.
// Each protocol carries the flag differently: a body field for OpenAI and
// Claude, the URL method for Gemini.
func IsStreamRequest(format types.RelayFormat, request any, path string) bool {
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		return typed.Stream != nil && *typed.Stream
	case *dto.OpenAIResponsesRequest:
		return typed.Stream != nil && *typed.Stream
	case *dto.ClaudeRequest:
		return typed.Stream != nil && *typed.Stream
	case *dto.GeminiChatRequest:
		return format == types.RelayFormatGemini && strings.HasSuffix(path, ":streamGenerateContent")
	default:
		return false
	}
}

// DecodeRequest parses a client body into the relaykit DTO for its protocol.
func DecodeRequest(format types.RelayFormat, body []byte) (any, error) {
	switch format {
	case types.RelayFormatOpenAI:
		return decodeInto[dto.GeneralOpenAIRequest](body)
	case types.RelayFormatOpenAIResponses:
		return decodeInto[dto.OpenAIResponsesRequest](body)
	case types.RelayFormatClaude:
		return decodeInto[dto.ClaudeRequest](body)
	case types.RelayFormatGemini:
		return decodeInto[dto.GeminiChatRequest](body)
	default:
		return nil, fmt.Errorf("no request decoder for protocol %q", format)
	}
}

// EncodeResponse serialises a converted DTO for the client.
func EncodeResponse(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	return data, nil
}

// SetModel rewrites the model name carried by a request DTO. Gemini has no
// model field — its model travels in the URL — so the caller keeps that name
// separately.
func SetModel(request any, model string) {
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		typed.Model = model
	case *dto.OpenAIResponsesRequest:
		typed.Model = model
	case *dto.ClaudeRequest:
		typed.Model = model
	}
}

// ClientModel reads the model name a client asked for. For Gemini the caller
// must supply the name parsed from the path.
func ClientModel(format types.RelayFormat, request any, pathModel string) string {
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		return typed.Model
	case *dto.OpenAIResponsesRequest:
		return typed.Model
	case *dto.ClaudeRequest:
		return typed.Model
	case *dto.GeminiChatRequest:
		return pathModel
	default:
		return ""
	}
}

func decodeInto[T any](body []byte) (*T, error) {
	value := new(T)
	if err := json.Unmarshal(body, value); err != nil {
		return nil, fmt.Errorf("decode %T: %w", value, err)
	}
	return value, nil
}
