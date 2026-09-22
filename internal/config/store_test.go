package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhangjl/newapi-box/relaykit/types"
)

func validConfig(baseURL string) *Config {
	return &Config{
		Listen:                 ":8080",
		Active:                 "default",
		ClaudeDefaultMaxTokens: 4096,
		Upstreams: []Upstream{
			{Name: "default", Format: types.RelayFormatOpenAI, BaseURL: baseURL, APIKey: "sk-secret"},
		},
	}
}

func TestSavePersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path, validConfig("https://a.example"))

	next := validConfig("https://b.example")
	next.Upstreams[0].ModelMap = map[string]string{"fast": "gpt-4o-mini"}
	if err := store.Save(next); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ActiveUpstream().BaseURL != "https://b.example" {
		t.Fatalf("base url = %q", reloaded.ActiveUpstream().BaseURL)
	}
	if reloaded.ActiveUpstream().ModelMap["fast"] != "gpt-4o-mini" {
		t.Fatalf("model map = %v", reloaded.ActiveUpstream().ModelMap)
	}
}

// A rejected config must not reach disk or the live snapshot, otherwise an
// operator typo could take down a working converter.
func TestSaveRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path, validConfig("https://good.example"))
	if err := store.Save(validConfig("https://good.example")); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	broken := validConfig("https://bad.example")
	broken.Upstreams[0].Format = "not-a-protocol"
	if err := store.Save(broken); err == nil {
		t.Fatal("expected validation to reject an unknown protocol")
	}

	if got := store.Current().ActiveUpstream().BaseURL; got != "https://good.example" {
		t.Fatalf("live config changed to %q after a rejected save", got)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ActiveUpstream().BaseURL != "https://good.example" {
		t.Fatalf("file changed to %q after a rejected save", reloaded.ActiveUpstream().BaseURL)
	}
}

// The store must not retain a caller-owned config: a caller that keeps a
// pointer and mutates it later must not be able to change live behaviour or
// what is on disk.
func TestStoreIsolatesCallerOwnedConfigs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	seeded := validConfig("https://a.example")
	store := NewStore(path, seeded)
	seeded.Upstreams[0].BaseURL = "https://mutated.example"
	if got := store.Current().ActiveUpstream().BaseURL; got != "https://a.example" {
		t.Fatalf("live base url = %q after mutating the seeded config", got)
	}

	if err := store.Save(validConfig("https://a.example")); err != nil {
		t.Fatal(err)
	}

	saved := validConfig("https://b.example")
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	saved.Upstreams[0].BaseURL = "https://mutated.example"

	if got := store.Current().ActiveUpstream().BaseURL; got != "https://b.example" {
		t.Fatalf("live base url = %q after mutating a saved config", got)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ActiveUpstream().BaseURL != "https://b.example" {
		t.Fatalf("file base url = %q after mutating a saved config", reloaded.ActiveUpstream().BaseURL)
	}
}

func TestCloneDeepCopiesMaps(t *testing.T) {
	original := validConfig("https://a.example")
	original.Upstreams[0].Headers = map[string]string{"x-a": "1"}
	original.Upstreams[0].Paths = map[string]string{"claude": "/v1/messages"}
	original.Upstreams[0].ModelMap = map[string]string{"a": "b"}

	clone := original.Clone()
	clone.Upstreams[0].Headers["x-a"] = "changed"
	clone.Upstreams[0].Paths["claude"] = "changed"
	clone.Upstreams[0].ModelMap["a"] = "changed"

	if original.Upstreams[0].Headers["x-a"] != "1" ||
		original.Upstreams[0].Paths["claude"] != "/v1/messages" ||
		original.Upstreams[0].ModelMap["a"] != "b" {
		t.Fatal("clone shares maps with the original")
	}
}

func TestLoadOrInitCreatesStarterConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	cfg, created, err := LoadOrInit(path)
	if err != nil {
		t.Fatalf("load or init: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a missing file")
	}
	if cfg.Listen != DefaultListen || cfg.ActiveUpstream().Format == "" {
		t.Fatalf("starter config is incomplete: %+v", cfg)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("starter config should not be written to disk")
	}
}

// A config that omits listen must land on the same address the starter config
// uses; otherwise the two entry paths disagree about the default port.
func TestEmptyListenFallsBackToTheDefault(t *testing.T) {
	if DefaultListen == "" {
		t.Fatal("DefaultListen must not be empty")
	}

	cfg := &Config{}
	cfg.Normalize()
	if cfg.Listen != DefaultListen {
		t.Fatalf("Normalize left listen as %q, want %q", cfg.Listen, DefaultListen)
	}

	explicit := &Config{Listen: ":19000"}
	explicit.Normalize()
	if explicit.Listen != ":19000" {
		t.Fatalf("Normalize overwrote an explicit listen: %q", explicit.Listen)
	}
}

func TestMaskedAPIKeyNeverRevealsTheKey(t *testing.T) {
	const key = "sk-abcdefghijklmnop"
	masked := MaskedAPIKey(key)

	if masked == key {
		t.Fatal("masked key equals the original")
	}
	if masked[:4] != key[:4] || masked[len(masked)-4:] != key[len(key)-4:] {
		t.Fatalf("masked key should keep only a 4-character preview: %q", masked)
	}
	for _, ch := range masked[4 : len(masked)-4] {
		if ch != '*' {
			t.Fatalf("masked middle contains %q, expected only '*'", ch)
		}
	}

	if MaskedAPIKey("") != "" {
		t.Fatal("empty key should mask to empty")
	}
	if got := MaskedAPIKey("short"); got != "*****" {
		t.Fatalf("short key mask = %q", got)
	}
}

func TestSaveWritesReadableJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path, validConfig("https://a.example"))
	if err := store.Save(validConfig("https://a.example")); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if decoded["listen"] != ":8080" {
		t.Fatalf("listen = %v", decoded["listen"])
	}
}

// Old config files carry a single "upstream" object; they must keep loading
// and the first save must upgrade them to the list shape.
func TestLegacySingleUpstreamMigratesIntoTheList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
		"listen": ":18888",
		"upstream": {"format": "openai", "base_url": "https://old.example", "api_key": "sk-old"}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load legacy config: %v", err)
	}
	if len(cfg.Upstreams) != 1 || cfg.Upstreams[0].Name != "default" {
		t.Fatalf("legacy upstream not migrated: %+v", cfg.Upstreams)
	}
	if cfg.Upstreams[0].BaseURL != "https://old.example" || cfg.Active != "default" {
		t.Fatalf("migrated upstream is wrong: active=%q upstreams=%+v", cfg.Active, cfg.Upstreams)
	}

	store := NewStore(path, cfg)
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save migrated config: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, stillThere := decoded["upstream"]; stillThere {
		t.Fatalf("save should write the list shape, got: %s", raw)
	}
	list, _ := decoded["upstreams"].([]any)
	if len(list) != 1 {
		t.Fatalf("upstreams = %v", decoded["upstreams"])
	}
}

// Activating another upstream is the whole feature: the pointer moves, the
// previously active upstream keeps its data, and traffic follows on the next
// request.
func TestActivationSwitchesTheLiveUpstream(t *testing.T) {
	cfg := validConfig("https://a.example")
	cfg.Upstreams = append(cfg.Upstreams, Upstream{
		Name: "second", Format: types.RelayFormatClaude, BaseURL: "https://b.example", APIKey: "sk-b",
	})

	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path, cfg)

	if got := store.Current().ActiveUpstream().Name; got != "default" {
		t.Fatalf("initial active = %q", got)
	}

	next := store.Current().Clone()
	next.Active = "second"
	if err := store.Save(next); err != nil {
		t.Fatalf("activate: %v", err)
	}

	live := store.Current().ActiveUpstream()
	if live.Name != "second" || live.Format != types.RelayFormatClaude {
		t.Fatalf("active upstream = %+v", live)
	}
	if first, ok := store.Current().FindUpstream("default"); !ok || first.BaseURL != "https://a.example" {
		t.Fatalf("the deactivated upstream lost data: %+v", first)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ActiveUpstream().Name != "second" {
		t.Fatalf("activation did not survive a reload: %q", reloaded.Active)
	}
}

func TestValidateRejectsBrokenMultiUpstreamShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "active upstream without a base url",
			mutate: func(c *Config) {
				c.Upstreams[0].BaseURL = ""
			},
		},
		{
			name: "active pointer to an unknown name",
			mutate: func(c *Config) {
				c.Active = "ghost"
			},
		},
		{
			name: "duplicate names",
			mutate: func(c *Config) {
				c.Upstreams[0].Name = "same"
				c.Upstreams = append(c.Upstreams, Upstream{Name: "same", Format: types.RelayFormatOpenAI, BaseURL: "https://x.example"})
			},
		},
		{
			name: "empty upstream name",
			mutate: func(c *Config) {
				c.Upstreams[0].Name = " "
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig("https://a.example")
			test.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s: expected a validation error", test.name)
			}
		})
	}
}

// Drafts must be savable: an operator fills in several upstreams over time,
// and only the active one has to be complete.
func TestInactiveUpstreamsMayStayIncomplete(t *testing.T) {
	cfg := validConfig("https://a.example")
	cfg.Upstreams = append(cfg.Upstreams, Upstream{Name: "draft"})
	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a blank draft must not block saving: %v", err)
	}
	if got := cfg.ActiveUpstream().BaseURL; got != "https://a.example" {
		t.Fatalf("active upstream = %q", got)
	}
}
