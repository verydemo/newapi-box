package main

import (
	"testing"

	"github.com/verydemo/newapi-box/internal/config"
)

func TestResolveListen(t *testing.T) {
	tests := []struct {
		name       string
		port       string
		listen     string
		fromConfig string
		want       string
		wantErr    bool
	}{
		{name: "neither flag keeps the config value", fromConfig: ":8080", want: ":8080"},
		{name: "port only", port: "9000", fromConfig: ":8080", want: ":9000"},
		{name: "port with a leading colon", port: ":9000", fromConfig: ":8080", want: ":9000"},
		{name: "port with surrounding spaces", port: " 9000 ", fromConfig: ":8080", want: ":9000"},
		{name: "listen only", listen: "127.0.0.1:9000", fromConfig: ":8080", want: "127.0.0.1:9000"},
		{name: "both flags are ambiguous", port: "9000", listen: "127.0.0.1:9001", fromConfig: ":8080", wantErr: true},
		{name: "non numeric port", port: "http", fromConfig: ":8080", wantErr: true},
		{name: "port zero is rejected", port: "0", fromConfig: ":8080", wantErr: true},
		{name: "port above the range", port: "70000", fromConfig: ":8080", wantErr: true},
		{name: "negative port", port: "-1", fromConfig: ":8080", wantErr: true},
		{name: "empty config falls back", fromConfig: "", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveListen(test.port, test.listen, test.fromConfig)
			if test.wantErr {
				if err == nil {
					t.Fatalf("resolveListen(%q, %q) succeeded with %q, want an error",
						test.port, test.listen, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveListen(%q, %q): %v", test.port, test.listen, err)
			}
			if got != test.want {
				t.Fatalf("resolveListen(%q, %q, %q) = %q, want %q",
					test.port, test.listen, test.fromConfig, got, test.want)
			}
		})
	}
}

func TestDisplayListenStripsTheHost(t *testing.T) {
	tests := map[string]string{
		"":               config.DefaultListen,
		":9000":          ":9000",
		"127.0.0.1:9000": ":9000",
		"0.0.0.0:9000":   ":9000",
		"[::1]:9000":     ":9000",
	}
	for input, want := range tests {
		if got := displayListen(input); got != want {
			t.Errorf("displayListen(%q) = %q, want %q", input, got, want)
		}
	}
}
