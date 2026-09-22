package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/verydemo/newapi-box/relaykit/dto"
	"github.com/verydemo/newapi-box/relaykit/types"
)

// doneSentinel terminates OpenAI-style streams. Claude and Gemini upstreams
// usually just close the connection, so it is treated as an optional marker
// rather than the only valid terminator.
const doneSentinel = "[DONE]"

// DecodeStreamEvent parses one upstream SSE frame into a relaykit DTO.
//
// done reports the end of the stream; the caller stops reading once it is
// true. Frames the converter cannot represent are skipped (done=false,
// value=nil, err=nil) rather than failing the whole stream, because upstreams
// routinely emit keep-alives and vendor-specific event types.
func DecodeStreamEvent(format types.RelayFormat, event Event) (value any, done bool, err error) {
	data := event.Data
	if data == doneSentinel {
		return nil, true, nil
	}
	if data == "" {
		return nil, false, nil
	}

	var target any
	switch format {
	case types.RelayFormatOpenAI:
		target = &dto.ChatCompletionsStreamResponse{}
	case types.RelayFormatOpenAIResponses:
		target = &dto.ResponsesStreamResponse{}
	case types.RelayFormatClaude:
		target = &dto.ClaudeResponse{}
	case types.RelayFormatGemini:
		target = &dto.GeminiChatResponse{}
	default:
		return nil, false, fmt.Errorf("no stream decoder for protocol %q", format)
	}

	if err := json.Unmarshal([]byte(data), target); err != nil {
		return nil, false, fmt.Errorf("decode %s stream event: %w", format, err)
	}
	return target, false, nil
}

// EncodeStreamEvent renders a converted DTO as downstream SSE frame(s).
//
// Claude Messages and OpenAI Responses frame every event with a matching
// `event:` line; OpenAI Chat and Gemini send bare data frames.
func EncodeStreamEvent(format types.RelayFormat, value any) ([]Event, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s stream event: %w", format, err)
	}
	payload := string(data)

	switch format {
	case types.RelayFormatOpenAI, types.RelayFormatGemini:
		return []Event{{Data: payload}}, nil
	case types.RelayFormatOpenAIResponses:
		return []Event{{Name: eventName(value), Data: payload}}, nil
	case types.RelayFormatClaude:
		name := eventName(value)
		if name == "" {
			return nil, nil
		}
		return []Event{{Name: name, Data: payload}}, nil
	default:
		return nil, fmt.Errorf("no stream encoder for protocol %q", format)
	}
}

// eventName reads the event name a DTO carries in its `type` field.
func eventName(value any) string {
	switch typed := value.(type) {
	case *dto.ResponsesStreamResponse:
		return typed.Type
	case *dto.ClaudeResponse:
		return typed.Type
	default:
		return ""
	}
}
