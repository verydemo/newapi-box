// Package admin serves the embedded operator console and its JSON API.
//
// The console exists so an operator can retarget the converter without editing
// JSON on disk and restarting. It is deliberately small: it edits the same
// config.Store the relay path reads, so there is exactly one source of truth.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/verydemo/newapi-box/relaykit/types"

	"github.com/verydemo/newapi-box/internal/config"
	"github.com/verydemo/newapi-box/internal/proxy"
)

// probeTimeout bounds a reachability check.
const probeTimeout = 25 * time.Second

// Admin serves both the console and the relay path.
type Admin struct {
	proxy   *proxy.Proxy
	started time.Time
	probe   *http.Client
}

func New(p *proxy.Proxy) *Admin {
	return &Admin{
		proxy:   p,
		started: time.Now(),
		probe:   &http.Client{Timeout: probeTimeout},
	}
}

// ServeHTTP routes admin traffic and delegates everything else to the relay.
func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/admin/api/"):
		a.serveAPI(w, r)
	case r.URL.Path == "/" || r.URL.Path == "/admin" || r.URL.Path == "/admin/":
		a.serveUI(w)
	default:
		a.proxy.ServeHTTP(w, r)
	}
}

func (a *Admin) serveAPI(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "invalid or missing admin key",
		})
		return
	}

	switch strings.TrimPrefix(r.URL.Path, "/admin/api/") {
	case "status":
		a.getStatus(w, r)
	case "config":
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			a.saveConfig(w, r)
			return
		}
		a.getConfig(w, r)
	case "test":
		a.testUpstream(w, r)
	case "events":
		a.getEvents(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown admin endpoint"})
	}
}

// authorized checks the admin key. Comparison is constant time so the console
// cannot be used as a key oracle.
func (a *Admin) authorized(r *http.Request) bool {
	expected := strings.TrimSpace(a.proxy.Store().Current().AdminKey)
	if expected == "" {
		// An unauthenticated console is a deliberate local-only choice; the
		// UI warns about it rather than silently accepting the risk.
		return true
	}

	candidates := []string{
		strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "),
		strings.TrimSpace(r.Header.Get("x-admin-key")),
	}
	for _, candidate := range candidates {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

// statusView is what the console header renders.
type statusView struct {
	ConfigPath     string              `json:"config_path"`
	Listen         string              `json:"listen"`
	UptimeSeconds  int64               `json:"uptime_seconds"`
	AuthRequired   bool                `json:"auth_required"`
	UpstreamName   string              `json:"upstream_name"`
	UpstreamFormat types.RelayFormat   `json:"upstream_format"`
	UpstreamURL    string              `json:"upstream_url"`
	UpstreamTotal  int                 `json:"upstream_total"`
	ModelMapSize   int                 `json:"model_map_size"`
	EventsTotal    uint64              `json:"events_total"`
	Formats        []types.RelayFormat `json:"formats"`
	Endpoints      []string            `json:"endpoints"`
}

var inboundEndpoints = []string{
	"POST /v1/chat/completions",
	"POST /v1/responses",
	"POST /v1/messages",
	"POST /v1beta/models/{model}:generateContent",
	"POST /v1beta/models/{model}:streamGenerateContent",
}

func (a *Admin) getStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := a.proxy.Store().Current()
	active := cfg.ActiveUpstream()
	writeJSON(w, http.StatusOK, statusView{
		ConfigPath:     a.proxy.Store().Path(),
		Listen:         cfg.Listen,
		UptimeSeconds:  int64(time.Since(a.started).Seconds()),
		AuthRequired:   strings.TrimSpace(cfg.AdminKey) != "",
		UpstreamName:   active.Name,
		UpstreamFormat: active.Format,
		UpstreamURL:    active.BaseURL,
		UpstreamTotal:  len(cfg.Upstreams),
		ModelMapSize:   len(active.ModelMap),
		EventsTotal:    a.proxy.EventsTotal(),
		Formats:        config.SupportedFormats(),
		Endpoints:      inboundEndpoints,
	})
}

// view is the console's config shape. Secrets are never returned: the console
// receives a masked preview and only sends a key back when the operator types
// a new one.
type view struct {
	Listen                 string         `json:"listen"`
	AdminKey               string         `json:"admin_key,omitempty"`
	AdminKeySet            bool           `json:"admin_key_set"`
	ClearAdminKey          bool           `json:"clear_admin_key,omitempty"`
	AccessKey              string         `json:"access_key,omitempty"`
	AccessKeySet           bool           `json:"access_key_set"`
	ClearAccessKey         bool           `json:"clear_access_key,omitempty"`
	ClaudeDefaultMaxTokens uint           `json:"claude_default_max_tokens"`
	Verbose                bool           `json:"verbose"`
	ActiveUpstream         string         `json:"active_upstream"`
	Upstreams              []upstreamView `json:"upstreams"`
}

type upstreamView struct {
	Name string `json:"name"`
	// OriginalName is the name this upstream had on the server. The console
	// sends it when the operator renamed the upstream, so an untouched key
	// field still finds the credential it belongs to.
	OriginalName string            `json:"original_name,omitempty"`
	Format       types.RelayFormat `json:"format"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key,omitempty"`
	APIKeySet    bool              `json:"api_key_set"`
	APIKeyMasked string            `json:"api_key_masked,omitempty"`
	ClearAPIKey  bool              `json:"clear_api_key,omitempty"`
	Timeout      string            `json:"timeout,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Paths        map[string]string `json:"paths,omitempty"`
	ModelMap     map[string]string `json:"model_map,omitempty"`
}

func viewOf(cfg *config.Config) view {
	out := view{
		Listen:                 cfg.Listen,
		AdminKeySet:            strings.TrimSpace(cfg.AdminKey) != "",
		AccessKeySet:           strings.TrimSpace(cfg.AccessKey) != "",
		ClaudeDefaultMaxTokens: cfg.ClaudeDefaultMaxTokens,
		Verbose:                cfg.Verbose,
		ActiveUpstream:         cfg.Active,
		Upstreams:              make([]upstreamView, 0, len(cfg.Upstreams)),
	}
	for _, u := range cfg.Upstreams {
		out.Upstreams = append(out.Upstreams, upstreamViewOf(u))
	}
	return out
}

func upstreamViewOf(u config.Upstream) upstreamView {
	return upstreamView{
		Name:         u.Name,
		Format:       u.Format,
		BaseURL:      u.BaseURL,
		APIKeySet:    strings.TrimSpace(u.APIKey) != "",
		APIKeyMasked: config.MaskedAPIKey(u.APIKey),
		Timeout:      u.Timeout,
		Headers:      u.Headers,
		Paths:        u.Paths,
		ModelMap:     u.ModelMap,
	}
}

// activeDraft returns the upstream the view marks as active, for probe and
// save handlers that act on "the one the console is pointing at".
func (v view) activeDraft() upstreamView {
	for _, u := range v.Upstreams {
		if u.Name == v.ActiveUpstream {
			return u
		}
	}
	if len(v.Upstreams) > 0 {
		return v.Upstreams[0]
	}
	return upstreamView{}
}

func (a *Admin) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, viewOf(a.proxy.Store().Current()))
}

// saveConfig applies the incoming view on top of the live config, persists it,
// and publishes it to the relay path.
func (a *Admin) saveConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("read request: %v", err)})
		return
	}

	var incoming view
	if err := json.Unmarshal(body, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("parse request: %v", err)})
		return
	}

	current := a.proxy.Store().Current()
	listenChanged := strings.TrimSpace(incoming.Listen) != current.Listen

	next := current.Clone()
	next.Listen = incoming.Listen
	next.Verbose = incoming.Verbose
	next.ClaudeDefaultMaxTokens = incoming.ClaudeDefaultMaxTokens

	switch {
	case incoming.ClearAdminKey:
		next.AdminKey = ""
	case strings.TrimSpace(incoming.AdminKey) != "":
		next.AdminKey = incoming.AdminKey
	case incoming.ClearAccessKey:
		next.AccessKey = ""
	case strings.TrimSpace(incoming.AccessKey) != "":
		next.AccessKey = incoming.AccessKey
	}

	// A blank key field means "leave it alone", so keys survive round trips
	// through the masked console. Match by name: a draft the operator never
	// touched keeps its stored key.
	prevKeys := make(map[string]string, len(current.Upstreams))
	for _, u := range current.Upstreams {
		prevKeys[u.Name] = u.APIKey
	}

	next.Upstreams = nil
	for _, iv := range incoming.Upstreams {
		// A rename must not orphan the stored credential, so fall back to the
		// name this upstream had on the server.
		previous := prevKeys[iv.Name]
		if previous == "" && iv.OriginalName != "" {
			previous = prevKeys[iv.OriginalName]
		}
		next.Upstreams = append(next.Upstreams, config.Upstream{
			Name:     iv.Name,
			Format:   iv.Format,
			BaseURL:  iv.BaseURL,
			Timeout:  iv.Timeout,
			Headers:  cleanMap(iv.Headers),
			Paths:    cleanMap(iv.Paths),
			ModelMap: cleanMap(iv.ModelMap),
			APIKey:   resolveAPIKey(iv, previous),
		})
	}
	next.Active = incoming.ActiveUpstream

	if err := a.proxy.Store().Save(next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := a.proxy.Apply(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	response := map[string]any{
		"config": viewOf(a.proxy.Store().Current()),
	}
	if listenChanged {
		// The listener address is bound at startup, so it cannot be changed
		// by a reload.
		response["restart_required"] = true
		response["notice"] = "listen 地址需要重启进程才能生效，其余配置已立即生效"
	}
	writeJSON(w, http.StatusOK, response)
}

// resolveAPIKey decides the stored key for an incoming upstream draft: an
// explicit clear wins, a typed key replaces, an empty field keeps the old one.
func resolveAPIKey(incoming upstreamView, previous string) string {
	switch {
	case incoming.ClearAPIKey:
		return ""
	case strings.TrimSpace(incoming.APIKey) != "":
		return incoming.APIKey
	default:
		return previous
	}
}

// probeOutcome is the verdict of a reachability check.
type probeOutcome struct {
	OK        bool   `json:"ok"`
	Method    string `json:"method"`
	URL       string `json:"url,omitempty"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Message   string `json:"message"`
	Body      string `json:"body,omitempty"`
}

// testUpstream probes the endpoint the converter will actually call.
//
// Probing a guessed /v1/models path is not portable. Gateways commonly expose
// only their inference routes, and those are POST-only, so a GET returns a
// route-not-found 404 that is indistinguishable from a wrong base URL. This
// instead posts an empty JSON object to the real conversion path with the real
// credentials: a validation error still proves the route exists and the key
// was accepted, and an empty body cannot be billed.
func (a *Admin) testUpstream(w http.ResponseWriter, r *http.Request) {
	cfg := a.proxy.Store().Current()
	stored := cfg.ActiveUpstream()

	format := stored.Format
	baseURL := stored.BaseURL
	apiKey := stored.APIKey
	paths := stored.Paths
	modelMap := stored.ModelMap

	// An operator may probe an unsaved draft by posting the form's config;
	// the probe then targets the upstream the view marks as active.
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(body) > 0 {
			var incoming view
			if err := json.Unmarshal(body, &incoming); err == nil {
				draft := incoming.activeDraft()
				if strings.TrimSpace(draft.BaseURL) != "" {
					// A blank key field means "use what is stored", so resolve
					// the baseline by name: probing upstream B must not borrow
					// the active upstream's credential.
					base := stored
					if named, ok := cfg.FindUpstream(draft.Name); ok {
						base = named
					}
					format = base.Format
					baseURL = draft.BaseURL
					apiKey = base.APIKey
					if draft.Format != "" {
						format = draft.Format
					}
					if key := strings.TrimSpace(draft.APIKey); key != "" {
						apiKey = key
					}
					paths = draft.Paths
					modelMap = draft.ModelMap
				}
			}
		}
	}

	upstream := config.Upstream{
		BaseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:   apiKey,
		Paths:    paths,
		ModelMap: modelMap,
	}
	if upstream.BaseURL == "" {
		writeJSON(w, http.StatusOK, probeOutcome{
			Method:  http.MethodPost,
			Message: "尚未配置上游地址",
		})
		return
	}

	path := upstream.PathFor(format)
	if format == types.RelayFormatGemini {
		path = strings.ReplaceAll(path, "{{model}}", probeModel(upstream))
	}
	target := upstream.BaseURL + path

	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		writeJSON(w, http.StatusOK, probeOutcome{Method: http.MethodPost, URL: target, Message: err.Error()})
		return
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range proxy.UpstreamAuthHeaders(format, apiKey) {
		request.Header.Set(name, value)
	}

	start := time.Now()
	response, err := a.probe.Do(request)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, probeOutcome{
			Method:    http.MethodPost,
			URL:       target,
			LatencyMS: latency,
			Message:   fmt.Sprintf("无法连接：%v", err),
		})
		return
	}
	defer response.Body.Close()

	snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	ok, message := classifyProbe(response.StatusCode)
	writeJSON(w, http.StatusOK, probeOutcome{
		OK:        ok,
		Method:    http.MethodPost,
		URL:       target,
		Status:    response.StatusCode,
		LatencyMS: latency,
		Message:   message,
		Body:      strings.TrimSpace(string(snippet)),
	})
}

// classifyProbe turns a status code into an operator-facing verdict. A
// validation error counts as success: it proves the route exists and the
// credential was accepted.
func classifyProbe(status int) (bool, string) {
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		return true, "上游可达，凭据有效"
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return true, "路由存在，凭据有效（空请求体被参数校验拒绝，符合预期）"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return false, "路由存在，但凭据被拒绝（检查 api_key）"
	case status == http.StatusNotFound:
		return false, "路由不存在（检查 base_url 与模型名；也可用 paths 覆盖路径）"
	case status == http.StatusMethodNotAllowed:
		return false, "路由存在但方法不被允许（该上游可能不支持所选协议）"
	case status == http.StatusTooManyRequests:
		return true, "上游可达，但已触发限流"
	case status >= 500:
		return false, fmt.Sprintf("上游返回服务端错误 %d（可达但异常）", status)
	default:
		return false, fmt.Sprintf("上游返回 %d", status)
	}
}

// probeModel supplies the model name for Gemini, whose model travels in the
// URL rather than the body.
func probeModel(upstream config.Upstream) string {
	for _, value := range upstream.ModelMap {
		if mapped := strings.TrimSpace(value); mapped != "" {
			return mapped
		}
	}
	return "probe"
}

func (a *Admin) getEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	writeJSON(w, http.StatusOK, map[string]any{
		"events": a.proxy.EventsSince(since),
		"latest": a.proxy.EventsTotal(),
	})
}

// cleanMap drops blank keys and trims whitespace, so an empty editor row does
// not become a real entry.
func cleanMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for key, value := range src {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
