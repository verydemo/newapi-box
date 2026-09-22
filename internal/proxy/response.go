package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/zhangjl/newapi-box/relaykit/dto"
	"github.com/zhangjl/newapi-box/relaykit/relayconvert"
	"github.com/zhangjl/newapi-box/relaykit/types"

	"github.com/zhangjl/newapi-box/internal/protocol"
)

// maxUpstreamResponseBody bounds a non-streaming upstream response.
const maxUpstreamResponseBody = 64 << 20

// usageExtractLimit caps the passthrough bodies we parse purely to populate
// the admin UI's token counters.
const usageExtractLimit = 4 << 20

// bufferedResponse handles a non-streaming upstream reply.
func (s *session) bufferedResponse(ctx context.Context, w http.ResponseWriter, response *http.Response) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamResponseBody))
	if err != nil {
		s.fail(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("read upstream response: %v", err))
		return
	}

	// Same protocol on both sides: forward the upstream bytes untouched so
	// vendor extensions the DTOs do not model survive the hop.
	if s.client == s.upstream {
		s.recordUsageFromBody(body)
		s.succeed(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	upstreamValue, err := decodeUpstreamResponse(s.upstream, body)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	result, err := relayconvert.ConvertResponse(ctx, s.meta, s.client, upstreamValue)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "conversion_error",
			fmt.Sprintf("cannot convert %s response to %s: %v", s.upstream, s.client, err))
		return
	}
	s.recordUsage(result.Usage)

	if s.rt.cfg.Verbose {
		log.Printf("convert response %s -> %s converter=%s quality=%s usage=%s",
			s.upstream, s.client, result.Converter, result.Quality, describeUsage(result.Usage))
	}

	encoded, err := protocol.EncodeResponse(result.Value)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "conversion_error", err.Error())
		return
	}

	s.succeed(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// streamResponse pumps upstream SSE frames through the kernel to the client.
//
// Every chunk is converted independently but shares one ResponseStreamState,
// which is where cross-event state lives: tool-call index remapping, usage
// accumulation, and the terminal events that only some protocols emit.
func (s *session) streamResponse(w http.ResponseWriter, response *http.Response) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, http.StatusInternalServerError, "conversion_error", "streaming is not supported by this server")
		return
	}

	state, err := relayconvert.NewResponseStreamState(s.upstream, s.client, relayconvert.ResponseStreamOptions{
		ID:           "conv-" + time.Now().UTC().Format("20060102150405.000000000"),
		Model:        s.upstreamModel,
		Created:      time.Now().Unix(),
		IncludeUsage: true,
	})
	if err != nil {
		s.fail(w, http.StatusBadRequest, "conversion_error",
			fmt.Sprintf("cannot convert %s stream to %s: %v", s.upstream, s.client, err))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// From here on the status is committed; failures can only be logged.
	s.event.Status = http.StatusOK

	writer := protocol.NewWriter(w)
	reader := protocol.NewReader(response.Body)
	var streamErr string

	emit := func(results []relayconvert.ResponseResult) bool {
		for _, result := range results {
			events, err := protocol.EncodeStreamEvent(s.client, result.Value)
			if err != nil {
				s.logVerbose("encode %s stream event: %v", s.client, err)
				continue
			}
			for _, event := range events {
				if err := writeEvent(writer, event); err != nil {
					// The client hung up; stop pumping rather than logging
					// an error per remaining chunk.
					return false
				}
			}
		}
		flusher.Flush()
		return true
	}

	for {
		event, ok, err := reader.Next()
		if !ok {
			if err != nil {
				streamErr = err.Error()
				s.logVerbose("read upstream stream: %v", err)
			}
			break
		}

		value, done, err := protocol.DecodeStreamEvent(s.upstream, event)
		if err != nil {
			// A single malformed frame must not kill the stream.
			s.logVerbose("decode %s stream event: %v", s.upstream, err)
			continue
		}
		if done {
			break
		}
		if value == nil {
			continue
		}

		results, err := relayconvert.ConvertStreamResponseChunk(context.Background(), s.meta, state, value)
		if err != nil {
			// Conversion failed mid-stream: the client already has a 200 and
			// partial content, so the only honest option is to stop.
			streamErr = err.Error()
			s.logVerbose("convert %s -> %s stream chunk: %v", s.upstream, s.client, err)
			break
		}
		if !emit(results) {
			s.finishStream(state, "client disconnected")
			return
		}
	}

	// Some protocols emit their terminal event here rather than upstream:
	// Claude's message_stop and Responses' response.completed are backfilled
	// by the kernel at finalize time.
	final, err := relayconvert.FinalizeStreamResponse(context.Background(), s.meta, state)
	if err != nil {
		streamErr = err.Error()
		s.logVerbose("finalize %s -> %s stream: %v", s.upstream, s.client, err)
	} else if !emit(final) {
		s.finishStream(state, "client disconnected")
		return
	}

	if s.rt.cfg.Verbose {
		log.Printf("stream complete %s -> %s usage=%s", s.upstream, s.client, describeUsage(state.Usage()))
	}

	// OpenAI Chat clients wait for an explicit sentinel; the other protocols
	// end at connection close.
	if s.client == types.RelayFormatOpenAI {
		_ = writer.WriteDone()
		flusher.Flush()
	}

	s.finishStream(state, streamErr)
}

func (s *session) finishStream(state *relayconvert.ResponseStreamState, streamErr string) {
	s.recordUsage(state.Usage())
	s.event.LatencyMS = time.Since(s.start).Milliseconds()
	if streamErr != "" {
		s.event.Error = streamErr
	}
}

func (s *session) recordUsage(usage *dto.Usage) {
	if usage == nil {
		return
	}
	s.event.PromptTokens = usage.PromptTokens
	s.event.CompletionTokens = usage.CompletionTokens
}

// recordUsageFromBody best-effort extracts usage from a passthrough body. It
// never affects the bytes forwarded to the client.
func (s *session) recordUsageFromBody(body []byte) {
	if len(body) == 0 || len(body) > usageExtractLimit {
		return
	}
	value, err := decodeUpstreamResponse(s.client, body)
	if err != nil {
		return
	}
	s.recordUsage(usageOf(value))
}

// usageOf reads the canonical usage a response DTO carries. relaykit keeps its
// own extractor unexported, so the host reads the four shapes directly.
func usageOf(value any) *dto.Usage {
	switch typed := value.(type) {
	case *dto.OpenAITextResponse:
		if typed == nil {
			return nil
		}
		// OpenAITextResponse carries usage by value.
		return &typed.Usage
	case *dto.OpenAIResponsesResponse:
		if typed == nil {
			return nil
		}
		return typed.Usage
	case *dto.ClaudeResponse:
		if typed == nil {
			return nil
		}
		return relayconvert.UsageFromClaudeAPIUsage(typed.Usage)
	case *dto.GeminiChatResponse:
		if typed == nil || !typed.HasUsageMetadata {
			return nil
		}
		metadata := typed.UsageMetadata
		return relayconvert.UsageFromGeminiMetadata(&metadata, 0)
	default:
		return nil
	}
}

func writeEvent(writer *protocol.Writer, event protocol.Event) error {
	if event.Name == "" {
		return writer.WriteData(event.Data)
	}
	return writer.WriteEvent(event.Name, event.Data)
}

// decodeUpstreamResponse parses an upstream body into its relaykit DTO.
func decodeUpstreamResponse(format types.RelayFormat, body []byte) (any, error) {
	var target any
	switch format {
	case types.RelayFormatOpenAI:
		target = &dto.OpenAITextResponse{}
	case types.RelayFormatOpenAIResponses:
		target = &dto.OpenAIResponsesResponse{}
	case types.RelayFormatClaude:
		target = &dto.ClaudeResponse{}
	case types.RelayFormatGemini:
		target = &dto.GeminiChatResponse{}
	default:
		return nil, fmt.Errorf("no response decoder for protocol %q", format)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", format, err)
	}
	return target, nil
}

func (s *session) logVerbose(format string, args ...any) {
	if s.rt.cfg.Verbose {
		log.Printf(format, args...)
	}
}

func describeUsage(usage *dto.Usage) string {
	if usage == nil {
		return "none"
	}
	return fmt.Sprintf("prompt=%d completion=%d total=%d",
		usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
}
