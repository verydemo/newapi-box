// Package config holds the universal converter's runtime configuration.
//
// The converter keeps several named upstreams but relays through exactly one
// at a time; switching is a config save, so it takes effect on the next
// request without restarting. Channel health, billing and auth belong to a
// gateway, not to a protocol converter; keeping the surface tiny is the whole
// point of the relaykit extraction.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/zhangjl/newapi-box/relaykit/types"
)

// Upstream is one configured target the converter can forward converted
// requests to.
type Upstream struct {
	// Name identifies the upstream. It is the key active_upstream refers to,
	// so it must be non-empty and unique within a config.
	Name string `json:"name"`

	// Format is the protocol the upstream speaks. Inbound requests in any
	// supported protocol are converted to this one.
	Format  types.RelayFormat `json:"format"`
	BaseURL string            `json:"base_url"`
	APIKey  string            `json:"api_key"`

	// Headers are added verbatim to every upstream request. Authentication
	// headers are derived from APIKey unless overridden here.
	Headers map[string]string `json:"headers,omitempty"`

	// Paths overrides the upstream path per protocol, keyed by relay format
	// ("openai", "claude", ...). Empty values fall back to the defaults.
	Paths map[string]string `json:"paths,omitempty"`

	// ModelMap rewrites the client's model name to the upstream's. Missing
	// entries pass the client model name through unchanged.
	ModelMap map[string]string `json:"model_map,omitempty"`

	// Timeout is a Go duration string. Empty means 5 minutes.
	Timeout string `json:"timeout,omitempty"`
}

// DefaultListen is the bind address used when neither the config file nor a
// startup flag supplies one.
const DefaultListen = ":18888"

// Config is the whole converter configuration.
type Config struct {
	Listen string `json:"listen"`
	// AdminKey, when set, must be presented as the console password.
	// See the admin package.
	AdminKey string `json:"admin_key,omitempty"`
	// AccessKey, when set, must be presented by callers of the data plane
	// (/v1/*). Independent of AdminKey so operator and caller credentials
	// can differ. Empty disables the check.
	AccessKey string `json:"access_key,omitempty"`

	// Upstreams are all configured targets. Exactly one is live at a time,
	// the one Active names; adding or editing the others changes nothing
	// until they are activated.
	Upstreams []Upstream `json:"upstreams,omitempty"`
	// Active names the live upstream. Empty falls back to the first entry.
	Active string `json:"active_upstream,omitempty"`

	// Upstream is the legacy single-upstream field. Load migrates it into
	// Upstreams and Normalize clears it, so old config files still parse and
	// the first save upgrades them; nothing writes this field anymore.
	Upstream *Upstream `json:"upstream,omitempty"`

	// ClaudeDefaultMaxTokens is injected when an inbound request is converted
	// to Claude Messages without max_tokens. The Claude API rejects requests
	// that omit it, so this must be non-zero whenever Claude is the active
	// upstream.
	ClaudeDefaultMaxTokens uint `json:"claude_default_max_tokens,omitempty"`

	// Verbose logs request/response conversion detail to stderr.
	Verbose bool `json:"verbose,omitempty"`
}

var supportedFormats = map[types.RelayFormat]bool{
	types.RelayFormatOpenAI:          true,
	types.RelayFormatOpenAIResponses: true,
	types.RelayFormatClaude:          true,
	types.RelayFormatGemini:          true,
}

// Load reads the JSON config at path and validates it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Normalize fills defaults, migrates the legacy single-upstream shape, and
// trims user-supplied whitespace. Both Load and Store.Save apply it so a
// config behaves identically whether it came from disk or from the admin UI.
func (c *Config) Normalize() {
	c.Listen = strings.TrimSpace(c.Listen)
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	c.AdminKey = strings.TrimSpace(c.AdminKey)
	c.AccessKey = strings.TrimSpace(c.AccessKey)
	c.Active = strings.TrimSpace(c.Active)

	if c.Upstream != nil {
		if len(c.Upstreams) == 0 {
			legacy := *c.Upstream
			legacy.Name = "default"
			c.Upstreams = []Upstream{legacy}
		}
		c.Upstream = nil
	}
	if len(c.Upstreams) == 0 {
		c.Upstreams = []Upstream{{Name: "default", Format: types.RelayFormatOpenAI, Timeout: "5m"}}
	}

	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		u.Name = strings.TrimSpace(u.Name)
		u.BaseURL = strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
		u.Timeout = strings.TrimSpace(u.Timeout)
	}
	if c.Active == "" {
		c.Active = c.Upstreams[0].Name
	}
	if c.ClaudeDefaultMaxTokens == 0 {
		c.ClaudeDefaultMaxTokens = 8192
	}
}

// Clone returns a deep copy. The store never hands out a pointer it still
// owns, so callers may mutate a clone freely.
func (c *Config) Clone() *Config {
	if c == nil {
		return &Config{}
	}
	clone := *c
	clone.Upstreams = make([]Upstream, len(c.Upstreams))
	for i, u := range c.Upstreams {
		u.Headers = cloneStringMap(u.Headers)
		u.Paths = cloneStringMap(u.Paths)
		u.ModelMap = cloneStringMap(u.ModelMap)
		clone.Upstreams[i] = u
	}
	if c.Upstream != nil {
		legacy := *c.Upstream
		legacy.Headers = cloneStringMap(c.Upstream.Headers)
		legacy.Paths = cloneStringMap(c.Upstream.Paths)
		legacy.ModelMap = cloneStringMap(c.Upstream.ModelMap)
		clone.Upstream = &legacy
	}
	return &clone
}

// ActiveUpstream returns the live upstream: the entry Active names, or the
// first entry when the pointer is unset or points nowhere. The fallback keeps
// a hand-edited config working; the console refuses to save that shape.
func (c *Config) ActiveUpstream() Upstream {
	if c == nil {
		return Upstream{}
	}
	if u, ok := c.FindUpstream(c.Active); ok {
		return u
	}
	if len(c.Upstreams) > 0 {
		return c.Upstreams[0]
	}
	return Upstream{}
}

// FindUpstream returns the named upstream.
func (c *Config) FindUpstream(name string) (Upstream, bool) {
	if c == nil {
		return Upstream{}, false
	}
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == name {
			return c.Upstreams[i], true
		}
	}
	return Upstream{}, false
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	maps.Copy(dst, src)
	return dst
}

// SupportedFormats lists the protocols the converter accepts and emits.
func SupportedFormats() []types.RelayFormat {
	return []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatOpenAIResponses,
		types.RelayFormatClaude,
		types.RelayFormatGemini,
	}
}

// MaskedAPIKey renders a key for display without revealing it. The admin UI
// shows this instead of the secret so a reachable-but-unauthenticated panel
// cannot be used to exfiltrate upstream credentials.
func MaskedAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", 6) + key[len(key)-4:]
}

// Default is the starter configuration used when no file exists yet. It points
// at nothing on purpose: the operator completes setup in the console, and
// nothing is written to disk until they save.
func Default() *Config {
	return &Config{
		Listen:                 DefaultListen,
		Active:                 "default",
		Upstreams:              []Upstream{{Name: "default", Format: types.RelayFormatOpenAI, Timeout: "5m"}},
		ClaudeDefaultMaxTokens: 8192,
	}
}

// LoadOrInit reads path, falling back to Default when the file is absent. The
// second return reports whether a starter config was substituted, which lets
// the caller explain the situation instead of failing to start.
func LoadOrInit(path string) (*Config, bool, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return Default(), true, nil
	}
	return nil, false, err
}

// Validate reports configuration that would fail at request time. Only the
// active upstream must be complete: the others may sit as drafts until the
// operator fills them in and activates them.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream is required")
	}

	seen := make(map[string]bool, len(c.Upstreams))
	for _, u := range c.Upstreams {
		if u.Name == "" {
			return fmt.Errorf("every upstream needs a name")
		}
		if seen[u.Name] {
			return fmt.Errorf("upstream name %q is duplicated", u.Name)
		}
		seen[u.Name] = true
		if u.Format != "" && !supportedFormats[u.Format] {
			return fmt.Errorf("upstream %q: format %q is not supported; use one of openai, openai_responses, claude, gemini", u.Name, u.Format)
		}
		if _, err := u.RequestTimeout(); err != nil {
			return err
		}
	}

	if _, ok := c.FindUpstream(c.Active); !ok {
		return fmt.Errorf("active_upstream %q does not match any configured upstream", c.Active)
	}
	active := c.ActiveUpstream()
	if !supportedFormats[active.Format] {
		return fmt.Errorf("upstream %q: format %q is not supported; use one of openai, openai_responses, claude, gemini", active.Name, active.Format)
	}
	if active.BaseURL == "" {
		return fmt.Errorf("upstream %q: base_url is required while it is the active upstream", active.Name)
	}
	if active.Format == types.RelayFormatClaude && c.ClaudeDefaultMaxTokens == 0 {
		return fmt.Errorf("claude_default_max_tokens is required while a Claude Messages upstream is active")
	}
	return nil
}

// RequestTimeout parses Upstream.Timeout, defaulting to five minutes.
func (u Upstream) RequestTimeout() (time.Duration, error) {
	if strings.TrimSpace(u.Timeout) == "" {
		return 5 * time.Minute, nil
	}
	timeout, err := time.ParseDuration(u.Timeout)
	if err != nil {
		return 0, fmt.Errorf("upstream.timeout %q is not a valid duration: %w", u.Timeout, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("upstream.timeout must be positive, got %s", timeout)
	}
	return timeout, nil
}

// MapModel applies ModelMap to a client-supplied model name.
func (u Upstream) MapModel(model string) string {
	if mapped, ok := u.ModelMap[model]; ok && strings.TrimSpace(mapped) != "" {
		return mapped
	}
	return model
}

// PathFor returns the upstream path for a protocol, honouring overrides.
func (u Upstream) PathFor(format types.RelayFormat) string {
	if override := strings.TrimSpace(u.Paths[string(format)]); override != "" {
		return override
	}
	switch format {
	case types.RelayFormatOpenAI:
		return "/v1/chat/completions"
	case types.RelayFormatOpenAIResponses:
		return "/v1/responses"
	case types.RelayFormatClaude:
		return "/v1/messages"
	case types.RelayFormatGemini:
		return "/v1beta/models/{{model}}:generateContent"
	default:
		return ""
	}
}

// GeminiStreamPathFor is the streaming counterpart of PathFor for Gemini,
// whose streaming endpoint is a distinct method rather than a flag.
func (u Upstream) GeminiStreamPathFor() string {
	if override := strings.TrimSpace(u.Paths[string(types.RelayFormatGemini)+"_stream"]); override != "" {
		return override
	}
	return "/v1beta/models/{{model}}:streamGenerateContent?alt=sse"
}
