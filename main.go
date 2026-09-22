// Command universal-convert exposes one upstream in any of four client
// protocols, converting requests, responses and streams through the relaykit
// kernel.
//
//	client: OpenAI Chat | OpenAI Responses | Claude Messages | Gemini
//	                          |
//	                   relaykit kernel
//	                          |
//	upstream:        one configured protocol, editable at runtime
//
// The operator console at / maintains the upstream configuration; saving it
// takes effect immediately because the relay path reads the same store.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/verydemo/newapi-box/internal/admin"
	"github.com/verydemo/newapi-box/internal/config"
	"github.com/verydemo/newapi-box/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	port := flag.String("port", "", "listen port, e.g. 9000 (binds all interfaces)")
	listen := flag.String("listen", "", "full listen address, e.g. 127.0.0.1:9000")
	flag.Parse()

	cfg, created, err := config.LoadOrInit(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "universal-convert: %v\n", err)
		os.Exit(1)
	}

	address, err := resolveListen(*port, *listen, cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "universal-convert: %v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	// The flags are a startup-only override: the console keeps showing and
	// editing the address stored in the config file.
	cfg.Listen = address

	store := config.NewStore(*configPath, cfg)

	relay, err := proxy.New(store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "universal-convert: %v\n", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: admin.New(relay),
		// Streaming replies are long-lived; these bound the idle periods
		// rather than the total request duration.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("universal-convert listening on %s", cfg.Listen)
	log.Printf("console: http://localhost%s/", displayListen(cfg.Listen))
	if created {
		log.Printf("no config at %s; starting with a blank upstream - finish setup in the console", *configPath)
	} else if problem := cfg.Validate(); problem != nil {
		// Start anyway: the console is how the operator fixes this.
		log.Printf("warning: current config is incomplete (%v)", problem)
		log.Printf("open the console to finish setup, then save")
	}
	if !created {
		active := cfg.ActiveUpstream()
		log.Printf("upstreams=%d active=%s protocol=%s base_url=%s",
			len(cfg.Upstreams), active.Name, active.Format, active.BaseURL)
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	go func() {
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "universal-convert: %v\n", serveErr)
			os.Exit(1)
		}
	}()

	<-shutdown
	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// displayListen makes ":18888" printable as a browsable address, and reduces a
// bound host to a loopback-friendly form.
func displayListen(listen string) string {
	if listen == "" {
		return config.DefaultListen
	}
	if _, port, err := net.SplitHostPort(listen); err == nil && port != "" {
		return ":" + port
	}
	return listen
}

// resolveListen picks the bind address. -port and -listen both override the
// config file, so a different port can be tried without editing JSON; they are
// mutually exclusive because accepting both would leave precedence ambiguous.
func resolveListen(port, listen, fromConfig string) (string, error) {
	port = strings.TrimSpace(port)
	listen = strings.TrimSpace(listen)

	switch {
	case port != "" && listen != "":
		return "", errors.New("use either -port or -listen, not both")
	case listen != "":
		return listen, nil
	case port == "":
		return fromConfig, nil
	}

	// Accept ":9000" as well as "9000"; the colon is easy to add by reflex.
	port = strings.TrimPrefix(port, ":")
	number, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("-port %q is not a number", port)
	}
	if number < 1 || number > 65535 {
		return "", fmt.Errorf("-port %d is outside the range 1-65535", number)
	}
	return ":" + port, nil
}
