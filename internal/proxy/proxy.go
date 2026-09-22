// Package proxy wires relaykit's conversion kernel to HTTP.
//
// The data path is deliberately one-directional and stateless:
//
//	client protocol  --decode-->  DTO  --relaykit-->  DTO  --encode-->  upstream
//	upstream protocol <--encode-- DTO <--relaykit--  DTO <--decode--  client
//
// relaykit owns every semantic decision between the two DTO hops. This package
// owns transport only: HTTP framing, headers, SSE, and error shapes.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/verydemo/newapi-box/relaykit/dto"
	"github.com/verydemo/newapi-box/relaykit/relayconvert"
	"github.com/verydemo/newapi-box/relaykit/relayconvert/convmeta"
	"github.com/verydemo/newapi-box/relaykit/types"

	"github.com/verydemo/newapi-box/internal/config"
	"github.com/verydemo/newapi-box/internal/protocol"
)

// maxRequestBody bounds client request bodies.
const maxRequestBody = 64 << 20

// mediaTimeout bounds a single outbound image fetch during conversion.
const mediaTimeout = 60 * time.Second

// Proxy converts and forwards one client protocol to one upstream protocol.
//
// The upstream is swappable at runtime: Apply publishes a new runtime snapshot
// atomically, so an operator can retarget the converter from the admin UI
// without dropping in-flight streams.
type Proxy struct {
	store  *config.Store
	rt     atomic.Pointer[runtime]
	events *eventLog
}

func New(store *config.Store) (*Proxy, error) {
	if store == nil {
		return nil, fmt.Errorf("config store is required")
	}

	// The media fetcher is independent of the upstream configuration, so it
	// keeps one client for the process lifetime rather than per revision.
	mediaClient := &http.Client{Timeout: mediaTimeout}
	relayconvert.SetMediaResolver(mediaResolver{client: mediaClient}.resolver())

	p := &Proxy{store: store, events: newEventLog()}
	if err := p.Apply(store.Current()); err != nil {
		return nil, err
	}
	return p, nil
}

// Apply publishes cfg as the live upstream. The previous runtime's idle
// connections are released once it is no longer reachable.
func (p *Proxy) Apply(cfg *config.Config) error {
	rt, err := newRuntime(cfg)
	if err != nil {
		return err
	}
	previous := p.rt.Swap(rt)
	if previous != nil {
		previous.close()
	}
	return nil
}

// EventsSince returns the conversion history newer than seq, for the console.
func (p *Proxy) EventsSince(seq uint64) []Event {
	return p.events.since(seq)
}

// EventsTotal is the number of conversions observed since start.
func (p *Proxy) EventsTotal() uint64 {
	return p.events.latestSeq()
}

// Store exposes the live configuration.
func (p *Proxy) Store() *config.Store {
	return p.store
}

// ServeHTTP serves one client request in any supported protocol.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		writeError(w, types.RelayFormatOpenAI, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}

	inbound, pathModel, ok := protocol.MatchInbound(r.URL.Path)
	if !ok {
		writeError(w, types.RelayFormatOpenAI, http.StatusNotFound, "not_found",
			fmt.Sprintf("unsupported path %q", r.URL.Path))
		return
	}

	rt := p.rt.Load()
	if rt == nil {
		writeError(w, inbound, http.StatusServiceUnavailable, "not_configured", "the converter has no upstream configured")
		return
	}

	if !rt.authorized(r) {
		writeError(w, inbound, http.StatusUnauthorized, "authentication_error", "invalid converter API key")
		return
	}

	request, err := decodeInbound(r, inbound)
	if err != nil {
		writeError(w, inbound, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	streaming := protocol.IsStreamRequest(inbound, request, r.URL.Path)
	clientModel := protocol.ClientModel(inbound, request, pathModel)
	if strings.TrimSpace(clientModel) == "" {
		writeError(w, inbound, http.StatusBadRequest, "invalid_request_error", "request does not name a model")
		return
	}

	session := &session{
		proxy:         p,
		rt:            rt,
		client:        inbound,
		upstream:      rt.up.Format,
		streaming:     streaming,
		clientModel:   clientModel,
		upstreamModel: rt.up.MapModel(clientModel),
		start:         start,
		event: Event{
			Time:          nowStamp(),
			Client:        string(inbound),
			Upstream:      string(rt.up.Format),
			Model:         clientModel,
			UpstreamModel: rt.up.MapModel(clientModel),
			Stream:        streaming,
		},
	}
	defer func() { p.events.append(session.event) }()

	session.handle(w, r, request)
}

// session carries the per-request state: the config snapshot it was admitted
// under, the conversion context, and the event being accumulated for the UI.
type session struct {
	proxy         *Proxy
	rt            *runtime
	client        types.RelayFormat
	upstream      types.RelayFormat
	streaming     bool
	clientModel   string
	upstreamModel string
	meta          *convmeta.Values
	start         time.Time

	event Event
}

func (s *session) handle(w http.ResponseWriter, r *http.Request, request any) {
	s.meta = s.newMeta()

	converted, err := s.convertRequest(r.Context(), request)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("cannot convert %s request to %s: %v", s.client, s.upstream, err))
		return
	}
	s.event.Converter = converted.Converter
	s.event.Diagnostics = len(converted.Diagnostics)

	if s.rt.cfg.Verbose {
		log.Printf("convert request %s -> %s model=%s->%s stream=%t diagnostics=%s",
			s.client, s.upstream, s.clientModel, s.upstreamModel, s.streaming,
			describeDiagnostics(converted.Diagnostics))
	}

	upstreamRequest, err := s.buildUpstreamRequest(r.Context(), converted.Value)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	response, err := s.rt.client.Do(upstreamRequest)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("upstream request failed: %v", err))
		return
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		s.relayUpstreamError(w, response)
		return
	}

	if s.streaming {
		s.streamResponse(w, response)
		return
	}
	s.bufferedResponse(r.Context(), w, response)
}

// fail writes an error in the caller's protocol and records it.
func (s *session) fail(w http.ResponseWriter, status int, code string, message string) {
	s.event.Status = status
	s.event.Error = message
	s.event.LatencyMS = time.Since(s.start).Milliseconds()
	writeError(w, s.client, status, code, message)
}

// succeed records a completed conversion.
func (s *session) succeed(status int) {
	s.event.Status = status
	s.event.LatencyMS = time.Since(s.start).Milliseconds()
}

// authorized enforces the optional data-plane API key. AccessKey is the
// caller-facing credential; AdminKey is still accepted so deployments that
// relied on one shared secret keep working.
func (r *runtime) authorized(request *http.Request) bool {
	accessKey := strings.TrimSpace(r.cfg.AccessKey)
	adminKey := strings.TrimSpace(r.cfg.AdminKey)
	if accessKey == "" && adminKey == "" {
		return true
	}
	presented := strings.TrimSpace(request.Header.Get("Authorization"))
	presented = strings.TrimPrefix(presented, "Bearer ")
	if presented == "" {
		presented = strings.TrimSpace(request.Header.Get("x-api-key"))
	}
	if presented == "" {
		presented = strings.TrimSpace(request.Header.Get("x-admin-key"))
	}
	if accessKey != "" && presented == accessKey {
		return true
	}
	return adminKey != "" && presented == adminKey
}

// decodeInbound reads and parses the client body.
func decodeInbound(r *http.Request, format types.RelayFormat) (any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		// Gemini requests still carry a body, but tolerate an empty one so
		// the decoder reports the protocol-specific error.
		body = []byte("{}")
	}
	return protocol.DecodeRequest(format, body)
}

// newMeta builds the conversion context relaykit reads. relaykit never
// consults global state, so every knob it needs is filled here.
func (s *session) newMeta() *convmeta.Values {
	defaultMaxTokens := int(s.rt.cfg.ClaudeDefaultMaxTokens)
	return &convmeta.Values{
		OriginModelName:     s.clientModel,
		UpstreamModelName:   s.upstreamModel,
		ChannelMetaAttached: true,
		IsStream:            s.streaming,
		Options: &convmeta.Options{
			Claude: convmeta.ClaudeOptions{
				// The Claude Messages API rejects a request without
				// max_tokens, so a cross-protocol hop must supply one.
				DefaultMaxTokens: func(string) int { return defaultMaxTokens },
			},
		},
	}
}

// convertRequest runs the kernel, or passes the DTO through when the client
// already speaks the upstream protocol.
func (s *session) convertRequest(ctx context.Context, request any) (*relayconvert.RequestResult, error) {
	if s.client == s.upstream {
		// Still normalise the model name the upstream will see.
		protocol.SetModel(request, s.upstreamModel)
		// 同协议直通也要补 reasoning.summary：Responses API 只在请求了
		// summary 时才返回推理内容，否则同一个模型换端点就会"丢思考"。
		request = relayconvert.EnsureResponsesReasoningSummary(request, s.client)
		return &relayconvert.RequestResult{Value: request, From: s.client, To: s.upstream}, nil
	}
	result, err := relayconvert.ConvertRequest(ctx, s.meta, s.upstream, request)
	if err != nil {
		return nil, err
	}
	// Gemini carries no model in its body, so the converted DTO needs it
	// written back from the URL the client called.
	protocol.SetModel(result.Value, s.upstreamModel)
	return result, nil
}

// buildUpstreamRequest renders the converted DTO into an HTTP request.
func (s *session) buildUpstreamRequest(ctx context.Context, payload any) (*http.Request, error) {
	if s.upstream != types.RelayFormatGemini {
		applyStreamFlag(s.upstream, payload, s.streaming)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", err)
	}

	target := s.rt.up.BaseURL + s.upstreamPath()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	if s.streaming {
		request.Header.Set("Accept", "text/event-stream")
	}
	s.applyAuthHeaders(request)

	for name, value := range s.rt.up.Headers {
		request.Header.Set(name, value)
	}
	return request, nil
}

// upstreamPath renders the per-protocol path, substituting the model for
// Gemini whose model lives in the URL rather than the body.
func (s *session) upstreamPath() string {
	if s.upstream != types.RelayFormatGemini {
		return s.rt.up.PathFor(s.upstream)
	}
	template := s.rt.up.PathFor(types.RelayFormatGemini)
	if s.streaming {
		template = s.rt.up.GeminiStreamPathFor()
	}
	return strings.ReplaceAll(template, "{{model}}", s.upstreamModel)
}

// applyAuthHeaders sets the authentication each upstream protocol expects.
func (s *session) applyAuthHeaders(request *http.Request) {
	for name, value := range UpstreamAuthHeaders(s.upstream, s.rt.up.APIKey) {
		request.Header.Set(name, value)
	}
}

// UpstreamAuthHeaders returns the authentication headers a protocol expects.
// Exported so the admin UI's connectivity probe authenticates exactly the way
// live traffic does, rather than reimplementing the rules.
func UpstreamAuthHeaders(format types.RelayFormat, apiKey string) map[string]string {
	switch format {
	case types.RelayFormatClaude:
		return map[string]string{
			"x-api-key":         apiKey,
			"anthropic-version": "2023-06-01",
		}
	case types.RelayFormatGemini:
		return map[string]string{"x-goog-api-key": apiKey}
	default:
		return map[string]string{"Authorization": "Bearer " + apiKey}
	}
}

// applyStreamFlag sets the streaming flag on targets that carry it in the body.
func applyStreamFlag(format types.RelayFormat, payload any, streaming bool) {
	switch typed := payload.(type) {
	case *dto.GeneralOpenAIRequest:
		typed.Stream = &streaming
		if streaming && typed.StreamOptions == nil {
			// Request a terminal usage frame; the converter reports it back
			// to the client in whatever shape its protocol uses.
			typed.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
		}
		if !streaming {
			typed.StreamOptions = nil
		}
	case *dto.OpenAIResponsesRequest:
		typed.Stream = &streaming
	case *dto.ClaudeRequest:
		typed.Stream = &streaming
	}
}

func describeDiagnostics(diagnostics []types.ConversionDiagnostic) string {
	if len(diagnostics) == 0 {
		return "none"
	}
	codes := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		codes = append(codes, diagnostic.Code)
	}
	return strings.Join(codes, ",")
}

// relayUpstreamError copies a non-200 upstream response to the client,
// preserving the status code so clients see retryable vs client errors.
func (s *session) relayUpstreamError(w http.ResponseWriter, response *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(response.StatusCode)

	s.event.Status = response.StatusCode
	s.event.LatencyMS = time.Since(s.start).Milliseconds()
	s.event.Error = fmt.Sprintf("upstream returned %d", response.StatusCode)

	if _, err := w.Write(body); err != nil && s.rt.cfg.Verbose {
		log.Printf("relay upstream error body: %v", err)
	}
}
