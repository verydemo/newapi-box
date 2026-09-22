package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/verydemo/newapi-box/relaykit/types"

	"github.com/verydemo/newapi-box/internal/config"
	"github.com/verydemo/newapi-box/internal/proxy"
)

// upstreamStub answers both the relay path and the connectivity probe, and
// tags every reply so a test can tell which upstream served it.
func upstreamStub(t *testing.T, tag string, seen *string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.URL.Path
		}
		if r.Method == http.MethodGet {
			// Probe endpoint.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{
			"id":"chatcmpl-%s","object":"chat.completion","created":1,"model":"%s",
			"choices":[{"index":0,"message":{"role":"assistant","content":"from %s"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}
		}`, tag, tag, tag))
	}))
	t.Cleanup(server.Close)
	return server
}

func newTestAdmin(t *testing.T, cfg *config.Config) (*Admin, *config.Store) {
	t.Helper()

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), cfg)
	relay, err := proxy.New(store)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return New(relay), store
}

func call(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return value
}

func baseConfig(baseURL string) *config.Config {
	return &config.Config{
		Listen:                 ":8080",
		Active:                 "default",
		ClaudeDefaultMaxTokens: 4096,
		Upstreams: []config.Upstream{
			{Name: "default", Format: types.RelayFormatOpenAI, BaseURL: baseURL, APIKey: "sk-secret-value"},
		},
	}
}

// The whole point of the console: retarget the converter while it runs.
func TestSavingConfigHotAppliesToRelayTraffic(t *testing.T) {
	var pathA string
	upstreamA := upstreamStub(t, "A", &pathA)

	admin, store := newTestAdmin(t, baseConfig(upstreamA.URL))

	first := call(t, admin, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "from A") {
		t.Fatalf("first relay: status=%d body=%s", first.Code, first.Body.String())
	}

	var pathB string
	upstreamB := upstreamStub(t, "B", &pathB)

	payload := fmt.Sprintf(`{"listen":":8080","verbose":false,"claude_default_max_tokens":4096,
		"active_upstream":"second",
		"upstreams":[
			{"name":"default","format":"openai","base_url":"https://a.example"},
			{"name":"second","format":"openai","base_url":%q,"api_key":"","model_map":{"gpt-4o":"gpt-4o"}}
		]}`, upstreamB.URL)
	saved := call(t, admin, http.MethodPut, "/admin/api/config", payload, nil)
	if saved.Code != http.StatusOK {
		t.Fatalf("save: status=%d body=%s", saved.Code, saved.Body.String())
	}

	second := call(t, admin, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "from B") {
		t.Fatalf("relay did not switch upstream: status=%d body=%s", second.Code, second.Body.String())
	}

	if store.Current().ActiveUpstream().Name != "second" || store.Current().ActiveUpstream().BaseURL != upstreamB.URL {
		t.Fatalf("store active upstream = %+v", store.Current().ActiveUpstream())
	}
}

// A blank secret field means "leave it alone", so the operator never has to
// retype a key just to change the base URL.
func TestEmptySecretFieldsPreserveStoredValues(t *testing.T) {
	admin, store := newTestAdmin(t, baseConfig("https://a.example"))

	payload := `{"listen":":8080","verbose":false,"claude_default_max_tokens":4096,
		"active_upstream":"default",
		"upstreams":[{"name":"default","format":"claude","base_url":"https://b.example","api_key":""}]}`
	recorder := call(t, admin, http.MethodPut, "/admin/api/config", payload, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	current := store.Current().ActiveUpstream()
	if current.APIKey != "sk-secret-value" {
		t.Fatalf("api key = %q, expected the stored value to survive", current.APIKey)
	}
	if current.BaseURL != "https://b.example" {
		t.Fatalf("base url = %q", current.BaseURL)
	}
}

// Keys are per upstream: editing upstream B must not disturb the key of A.
func TestEmptyKeyFieldOnlyPreservesTheSameUpstream(t *testing.T) {
	cfg := baseConfig("https://a.example")
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
		Name: "second", Format: types.RelayFormatOpenAI, BaseURL: "https://b.example", APIKey: "sk-second",
	})
	admin, store := newTestAdmin(t, cfg)

	payload := `{"listen":":8080","claude_default_max_tokens":4096,"active_upstream":"default",
		"upstreams":[
			{"name":"default","format":"openai","base_url":"https://a.changed"},
			{"name":"second","format":"openai","base_url":"https://b.example","api_key":"sk-replaced"}
		]}`
	recorder := call(t, admin, http.MethodPut, "/admin/api/config", payload, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	upstreams := store.Current().Upstreams
	if upstreams[0].APIKey != "sk-secret-value" {
		t.Fatalf("untouched upstream key = %q, expected it to survive", upstreams[0].APIKey)
	}
	if upstreams[1].APIKey != "sk-replaced" {
		t.Fatalf("typed key = %q, expected the replacement", upstreams[1].APIKey)
	}
}

func TestClearSecretsRequiresExplicitFlags(t *testing.T) {
	admin, store := newTestAdmin(t, baseConfig("https://a.example"))

	payload := `{"listen":":8080","claude_default_max_tokens":4096,
		"clear_admin_key":true,"active_upstream":"default",
		"upstreams":[{"name":"default","format":"openai","base_url":"https://a.example","clear_api_key":true}]}`
	recorder := call(t, admin, http.MethodPut, "/admin/api/config", payload, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if store.Current().ActiveUpstream().APIKey != "" {
		t.Fatal("api key should have been cleared")
	}
}

// The console must never be a way to read the upstream credential.
func TestConfigResponseNeverLeaksSecrets(t *testing.T) {
	admin, _ := newTestAdmin(t, baseConfig("https://a.example"))

	recorder := call(t, admin, http.MethodGet, "/admin/api/config", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}

	body := recorder.Body.String()
	if strings.Contains(body, "sk-secret-value") {
		t.Fatalf("config response leaked the api key: %s", body)
	}

	decoded := decodeBody(t, recorder)
	upstreams, _ := decoded["upstreams"].([]any)
	if len(upstreams) != 1 {
		t.Fatalf("upstreams = %v", decoded["upstreams"])
	}
	upstream, _ := upstreams[0].(map[string]any)
	if upstream["api_key_set"] != true {
		t.Fatalf("api_key_set = %v", upstream["api_key_set"])
	}
	masked, _ := upstream["api_key_masked"].(string)
	if !strings.Contains(masked, "*") {
		t.Fatalf("api_key_masked = %q", masked)
	}
}

func TestAdminKeyIsRequiredWhenConfigured(t *testing.T) {
	cfg := baseConfig("https://a.example")
	cfg.AdminKey = "console-secret"
	admin, _ := newTestAdmin(t, cfg)

	unauthorized := call(t, admin, http.MethodGet, "/admin/api/status", "", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", unauthorized.Code)
	}

	authorized := call(t, admin, http.MethodGet, "/admin/api/status", "",
		map[string]string{"Authorization": "Bearer console-secret"})
	if authorized.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", authorized.Code, authorized.Body.String())
	}

	// The x-admin-key header is accepted too, for curl users.
	viaHeader := call(t, admin, http.MethodGet, "/admin/api/status", "",
		map[string]string{"x-admin-key": "console-secret"})
	if viaHeader.Code != http.StatusOK {
		t.Fatalf("status=%d", viaHeader.Code)
	}

	wrong := call(t, admin, http.MethodGet, "/admin/api/status", "",
		map[string]string{"Authorization": "Bearer console-secre"})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 for a near-miss key", wrong.Code)
	}
}

func TestStatusReportsLiveConfiguration(t *testing.T) {
	cfg := baseConfig("https://a.example")
	cfg.Upstreams[0].ModelMap = map[string]string{"a": "b"}
	admin, _ := newTestAdmin(t, cfg)

	recorder := call(t, admin, http.MethodGet, "/admin/api/status", "", nil)
	decoded := decodeBody(t, recorder)

	if decoded["upstream_name"] != "default" {
		t.Fatalf("upstream_name = %v", decoded["upstream_name"])
	}
	if decoded["upstream_format"] != "openai" {
		t.Fatalf("upstream_format = %v", decoded["upstream_format"])
	}
	if decoded["upstream_url"] != "https://a.example" {
		t.Fatalf("upstream_url = %v", decoded["upstream_url"])
	}
	if decoded["upstream_total"] != float64(1) {
		t.Fatalf("upstream_total = %v", decoded["upstream_total"])
	}
	if decoded["model_map_size"] != float64(1) {
		t.Fatalf("model_map_size = %v", decoded["model_map_size"])
	}
	if decoded["auth_required"] != false {
		t.Fatalf("auth_required = %v", decoded["auth_required"])
	}
	endpoints, _ := decoded["endpoints"].([]any)
	if len(endpoints) == 0 {
		t.Fatal("status should advertise the inbound endpoints")
	}
}

// Changing the bind address cannot take effect without a restart, and the API
// says so rather than silently ignoring it.
func TestListenChangeReportsRestartRequired(t *testing.T) {
	admin, _ := newTestAdmin(t, baseConfig("https://a.example"))

	payload := `{"listen":":9999","claude_default_max_tokens":4096,"active_upstream":"default",
		"upstreams":[{"name":"default","format":"openai","base_url":"https://a.example"}]}`
	recorder := call(t, admin, http.MethodPut, "/admin/api/config", payload, nil)
	decoded := decodeBody(t, recorder)

	if decoded["restart_required"] != true {
		t.Fatalf("restart_required = %v", decoded["restart_required"])
	}
}

func TestInvalidConfigIsRejectedWithAReason(t *testing.T) {
	admin, store := newTestAdmin(t, baseConfig("https://a.example"))

	payload := `{"listen":":8080","claude_default_max_tokens":4096,"active_upstream":"default",
		"upstreams":[{"name":"default","format":"telepathy","base_url":"https://a.example"}]}`
	recorder := call(t, admin, http.MethodPut, "/admin/api/config", payload, nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "telepathy") {
		t.Fatalf("error should name the rejected value: %s", recorder.Body.String())
	}
	if store.Current().ActiveUpstream().Format != types.RelayFormatOpenAI {
		t.Fatal("a rejected save must not change the live config")
	}
}

// Activating an upstream that is still a draft must fail with a reason: the
// converter would otherwise route traffic into a half-filled config.
func TestActivatingAnIncompleteUpstreamIsRejected(t *testing.T) {
	cfg := baseConfig("https://a.example")
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{Name: "draft", Format: types.RelayFormatOpenAI})
	admin, store := newTestAdmin(t, cfg)

	payload := `{"listen":":8080","claude_default_max_tokens":4096,"active_upstream":"draft",
		"upstreams":[
			{"name":"default","format":"openai","base_url":"https://a.example"},
			{"name":"draft","format":"openai","base_url":""}
		]}`
	recorder := call(t, admin, http.MethodPut, "/admin/api/config", payload, nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if store.Current().ActiveUpstream().Name != "default" {
		t.Fatalf("active upstream = %q, the rejected save must not switch", store.Current().Active)
	}
}

func TestProbeReportsReachableUpstream(t *testing.T) {
	upstream := upstreamStub(t, "A", nil)
	admin, _ := newTestAdmin(t, baseConfig(upstream.URL))

	recorder := call(t, admin, http.MethodPost, "/admin/api/test", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	decoded := decodeBody(t, recorder)
	if decoded["ok"] != true {
		t.Fatalf("probe failed: %v", decoded)
	}
	if decoded["status"] != float64(200) {
		t.Fatalf("probe status = %v", decoded["status"])
	}
}

func TestProbeReportsUnreachableUpstream(t *testing.T) {
	// Port 1 is reserved and refuses connections.
	admin, _ := newTestAdmin(t, baseConfig("http://127.0.0.1:1"))

	recorder := call(t, admin, http.MethodPost, "/admin/api/test", "", nil)
	decoded := decodeBody(t, recorder)

	if decoded["ok"] != false {
		t.Fatalf("expected ok=false, got %v", decoded)
	}
	if decoded["message"] == nil {
		t.Fatal("an unreachable upstream should explain itself")
	}
}

// A probe can target a draft, so an operator can validate before saving.
func TestProbeCanUseADraftConfig(t *testing.T) {
	upstream := upstreamStub(t, "A", nil)
	admin, _ := newTestAdmin(t, baseConfig("http://127.0.0.1:1"))

	draft := fmt.Sprintf(`{"active_upstream":"default","upstreams":[{"name":"default","format":"openai","base_url":%q,"api_key":"k"}]}`, upstream.URL)
	recorder := call(t, admin, http.MethodPost, "/admin/api/test", draft, nil)
	decoded := decodeBody(t, recorder)

	if decoded["ok"] != true {
		t.Fatalf("draft probe should reach the stubbed upstream: %v", decoded)
	}
}

func TestEventsRecordRelayTraffic(t *testing.T) {
	upstream := upstreamStub(t, "A", nil)
	admin, _ := newTestAdmin(t, baseConfig(upstream.URL))

	call(t, admin, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)

	recorder := call(t, admin, http.MethodGet, "/admin/api/events?since=0", "", nil)
	decoded := decodeBody(t, recorder)

	events, _ := decoded["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("expected one recorded event, got %d", len(events))
	}
	event, _ := events[0].(map[string]any)
	if event["client"] != "openai" || event["status"] != float64(200) {
		t.Fatalf("unexpected event: %v", event)
	}
	if event["prompt_tokens"] != float64(1) || event["completion_tokens"] != float64(2) {
		t.Fatalf("event should carry usage: %v", event)
	}

	// Polling with the latest sequence must return nothing new.
	latest := decoded["latest"]
	again := call(t, admin, http.MethodGet, fmt.Sprintf("/admin/api/events?since=%v", latest), "", nil)
	after := decodeBody(t, again)
	if list, _ := after["events"].([]any); len(list) != 0 {
		t.Fatalf("incremental poll returned %d stale events", len(list))
	}
}

func TestEventsRecordFailures(t *testing.T) {
	admin, _ := newTestAdmin(t, baseConfig("http://127.0.0.1:1"))

	call(t, admin, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)

	recorder := call(t, admin, http.MethodGet, "/admin/api/events?since=0", "", nil)
	decoded := decodeBody(t, recorder)
	events, _ := decoded["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("expected one event, got %d", len(events))
	}
	event, _ := events[0].(map[string]any)
	if event["status"] != float64(http.StatusBadGateway) {
		t.Fatalf("status = %v", event["status"])
	}
	if event["error"] == nil {
		t.Fatal("a failed conversion should record why")
	}
}

func TestUnknownAdminEndpointReturns404(t *testing.T) {
	admin, _ := newTestAdmin(t, baseConfig("https://a.example"))
	recorder := call(t, admin, http.MethodGet, "/admin/api/nope", "", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestConsoleIsServedAtRootAndSlashAdmin(t *testing.T) {
	admin, _ := newTestAdmin(t, baseConfig("https://a.example"))

	for _, path := range []string{"/", "/admin"} {
		recorder := call(t, admin, http.MethodGet, path, "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status=%d", path, recorder.Code)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "newapi") {
			t.Fatalf("%s: console HTML missing", path)
		}
		// The console is useless if it cannot reach the config API.
		if !strings.Contains(body, "/admin/api/config") {
			t.Fatalf("%s: console should call the config API", path)
		}
		if !strings.Contains(body, "id=\"cards\"") {
			t.Fatalf("%s: console should render the upstream list", path)
		}
	}
}

// Relay paths must still work through the combined handler.
func TestRelayPathsStillRouteThrough(t *testing.T) {
	var seenPath string
	upstream := upstreamStub(t, "A", &seenPath)
	admin, _ := newTestAdmin(t, baseConfig(upstream.URL))

	recorder := call(t, admin, http.MethodPost, "/v1/messages",
		`{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if seenPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", seenPath)
	}
}

func TestCleanMapDropsBlankRows(t *testing.T) {
	result := cleanMap(map[string]string{
		"  ":      "orphan",
		" x-key ": " value ",
		"empty":   "",
	})
	if _, present := result[""]; present {
		t.Fatal("blank keys must be dropped")
	}
	if result["x-key"] != "value" {
		t.Fatalf("result = %v", result)
	}
	if _, present := result["empty"]; !present {
		// A key with an intentionally blank value is still a real entry.
		t.Fatal("a named key with a blank value should be kept")
	}
	if cleanMap(map[string]string{" ": "x"}) != nil {
		t.Fatal("a map of only blank keys should collapse to nil")
	}
}

// Regression: the probe used to GET a guessed /v1/models path. Gateways that
// expose only POST inference routes answer that with a route-not-found 404,
// which is indistinguishable from a wrong base URL — a correct configuration
// was reported as broken.
func TestProbeUsesTheRealEndpointNotAModelsGuess(t *testing.T) {
	var gotPath, gotMethod string

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"Not Found","message":"Route `+r.URL.Path+` not found"}`)
			return
		}
		if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"Missing API key"}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"model is required"}`)
	}))
	t.Cleanup(gateway.Close)

	cfg := baseConfig(gateway.URL)
	cfg.Upstreams[0].Format = types.RelayFormatOpenAIResponses
	admin, _ := newTestAdmin(t, cfg)

	recorder := call(t, admin, http.MethodPost, "/admin/api/test", "", nil)
	decoded := decodeBody(t, recorder)

	if decoded["ok"] != true {
		t.Fatalf("a validation error proves route + credential, so the probe must pass: %v", decoded)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("probe method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("probe path = %q, want the configured protocol's real path", gotPath)
	}
}

func TestProbeReportsARejectedCredential(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Missing API key"}`)
	}))
	t.Cleanup(gateway.Close)

	admin, _ := newTestAdmin(t, baseConfig(gateway.URL))
	decoded := decodeBody(t, call(t, admin, http.MethodPost, "/admin/api/test", "", nil))

	if decoded["ok"] != false {
		t.Fatalf("a rejected credential must fail the probe: %v", decoded)
	}
	if message, _ := decoded["message"].(string); !strings.Contains(message, "凭据") {
		t.Fatalf("message should name the credential problem, got %q", message)
	}
}

func TestProbeHonoursTheConfiguredPathOverride(t *testing.T) {
	var gotPath string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gateway.Close)

	cfg := baseConfig(gateway.URL)
	cfg.Upstreams[0].Paths = map[string]string{"openai": "/custom/chat"}
	admin, _ := newTestAdmin(t, cfg)

	call(t, admin, http.MethodPost, "/admin/api/test", "", nil)
	if gotPath != "/custom/chat" {
		t.Fatalf("probe path = %q, want the override", gotPath)
	}
}

func TestProbeSubstitutesTheModelForGemini(t *testing.T) {
	var gotPath string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(gateway.Close)

	cfg := baseConfig(gateway.URL)
	cfg.Upstreams[0].Format = types.RelayFormatGemini
	cfg.Upstreams[0].ModelMap = map[string]string{"alias": "gemini-2.0-flash"}
	admin, _ := newTestAdmin(t, cfg)

	call(t, admin, http.MethodPost, "/admin/api/test", "", nil)
	if !strings.Contains(gotPath, "gemini-2.0-flash") {
		t.Fatalf("gemini probe path = %q, want the resolved model", gotPath)
	}
	if strings.Contains(gotPath, "{{model}}") {
		t.Fatalf("gemini probe path still holds the placeholder: %q", gotPath)
	}
}

// Probing upstream B from the list must authenticate as B. A blank key field
// in the draft means "use the stored one", and the stored one belongs to the
// named upstream, not to whichever upstream happens to be active.
func TestProbeUsesTheNamedUpstreamsStoredCredential(t *testing.T) {
	var gotAuth string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gateway.Close)

	cfg := baseConfig("https://a.example")
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
		Name: "second", Format: types.RelayFormatOpenAI, BaseURL: gateway.URL, APIKey: "sk-second-key",
	})
	admin, _ := newTestAdmin(t, cfg)

	draft := fmt.Sprintf(`{"active_upstream":"second","upstreams":[
		{"name":"second","format":"openai","base_url":%q,"api_key":""}]}`, gateway.URL)
	recorder := call(t, admin, http.MethodPost, "/admin/api/test", draft, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gotAuth != "Bearer sk-second-key" {
		t.Fatalf("probe authenticated as %q, want the named upstream's key", gotAuth)
	}
}

// Renaming is not identity: an untouched key field must still find the
// credential, which is why the console reports the previous name.
func TestRenamingAnUpstreamKeepsItsStoredKey(t *testing.T) {
	admin, store := newTestAdmin(t, baseConfig("https://a.example"))

	payload := `{"listen":":8080","claude_default_max_tokens":4096,"active_upstream":"renamed",
		"upstreams":[{"name":"renamed","original_name":"default","format":"openai",
			"base_url":"https://a.example","api_key":""}]}`
	recorder := call(t, admin, http.MethodPut, "/admin/api/config", payload, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	live := store.Current().ActiveUpstream()
	if live.Name != "renamed" {
		t.Fatalf("name = %q", live.Name)
	}
	if live.APIKey != "sk-secret-value" {
		t.Fatalf("api key = %q, renaming must not drop it", live.APIKey)
	}
}

func TestClassifyProbe(t *testing.T) {
	// A validation error is the expected answer to an empty probe body, so it
	// must count as reachable; only auth, routing and server faults fail.
	tests := []struct {
		status int
		ok     bool
	}{
		{http.StatusOK, true},
		{http.StatusCreated, true},
		{http.StatusBadRequest, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusTooManyRequests, true},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusMethodNotAllowed, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusFound, false},
	}

	for _, test := range tests {
		ok, message := classifyProbe(test.status)
		if ok != test.ok {
			t.Errorf("classifyProbe(%d) ok = %v, want %v", test.status, ok, test.ok)
		}
		if strings.TrimSpace(message) == "" {
			t.Errorf("classifyProbe(%d) must explain itself", test.status)
		}
	}
}
