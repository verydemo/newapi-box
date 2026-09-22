package proxy

import (
	"net"
	"net/http"
	"time"

	"github.com/verydemo/newapi-box/internal/config"
)

// runtime is an immutable snapshot of everything a request needs to reach the
// upstream: the config, the deadline, and the HTTP client built from them.
//
// A request takes one snapshot at entry and uses it throughout. An in-flight
// conversion therefore keeps the upstream it started against even if the
// operator saves a new config mid-stream, while the next request picks up the
// change immediately.
type runtime struct {
	cfg *config.Config
	// up is the live upstream at snapshot time. It is resolved once so a
	// request never mixes fields from two upstreams mid-activation.
	up        config.Upstream
	timeout   time.Duration
	client    *http.Client
	transport *http.Transport
}

func newRuntime(cfg *config.Config) (*runtime, error) {
	up := cfg.ActiveUpstream()
	timeout, err := up.RequestTimeout()
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
		// Bound connect + headers so a dead upstream fails fast. Response
		// bodies are deliberately left without a total deadline: a streaming
		// completion may legitimately run for minutes after its headers.
		ResponseHeaderTimeout: timeout,
	}

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// A redirected relay would silently drop the request body.
			return http.ErrUseLastResponse
		},
	}

	return &runtime{cfg: cfg, up: up, timeout: timeout, client: client, transport: transport}, nil
}

// close releases the idle connections of a superseded runtime.
func (r *runtime) close() {
	if r == nil || r.transport == nil {
		return
	}
	r.transport.CloseIdleConnections()
}
