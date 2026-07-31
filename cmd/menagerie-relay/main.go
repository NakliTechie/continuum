// Command menagerie-relay runs a Menagerie relay: it exposes a WebSocket the
// browser (or a supervisor agent) connects to and, from P3 on, manages PTYs for
// spawned coding agents.
//
// Usage:
//
//	menagerie-relay serve            start the relay (creates config on first run;
//	                                 copies the registration token to the clipboard)
//	menagerie-relay service install  run the relay always-on (launchd / systemd)
//	menagerie-relay init             generate ~/.menagerie/relay.toml + a token only
//	menagerie-relay token print      re-print the registration token
//	menagerie-relay token rotate     generate a new registration token
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NakliTechie/menagerie/relay-go/internal/config"
	"github.com/NakliTechie/menagerie/relay-go/internal/server"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("menagerie-relay: ")

	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	path, err := config.DefaultPath()
	if err != nil {
		fatal(err)
	}

	switch cmd {
	case "init":
		cmdInit(path)
	case "serve":
		cmdServe(path)
	case "token":
		cmdToken(path, args[1:])
	case "service":
		cmdService(path, args[1:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func cmdInit(path string) {
	if config.Exists(path) {
		fatalf("config already exists at %s (use `menagerie-relay token rotate` to change the token)", path)
	}
	cfg, err := config.Default()
	if err != nil {
		fatal(err)
	}
	if err := config.Save(path, cfg); err != nil {
		fatal(err)
	}
	fmt.Printf("Created %s\n", path)
	fmt.Printf("Relay name: %s\n", cfg.Name)
	fmt.Printf("Listening:  %s  (edit to 0.0.0.0:PORT to expose beyond localhost)\n\n", cfg.Listen)
	announceToken(cfg.RegistrationToken)
	fmt.Println("Then run `menagerie-relay serve` (or `menagerie-relay service install` to keep it always-on).")
}

func cmdServe(path string) {
	cfg := ensureConfig(path) // creates config on first run so `serve` is the only command needed
	if cfg.RegistrationToken == "" {
		fatalf("config has an empty registration_token — run `menagerie-relay token rotate`")
	}
	if isInteractive() {
		announceToken(cfg.RegistrationToken)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		secure := cfg.TLSCert != "" && cfg.TLSKey != ""
		log.Printf("relay %q listening on %s (tls=%v, origins=%v)", cfg.Name, cfg.Listen, secure, cfg.AllowedOrigins)
		var serveErr error
		if secure {
			serveErr = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			fatal(serveErr)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func cmdToken(path string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "print":
		cfg, err := config.Load(path)
		if err != nil {
			fatal(err)
		}
		if isInteractive() {
			announceToken(cfg.RegistrationToken) // copies to clipboard for a human
		} else {
			fmt.Println(cfg.RegistrationToken) // bare, pipe-friendly
		}
	case "rotate":
		cfg, err := config.Load(path)
		if err != nil {
			fatal(err)
		}
		tok, err := config.GenerateToken()
		if err != nil {
			fatal(err)
		}
		cfg.RegistrationToken = tok
		if err := config.Save(path, cfg); err != nil {
			fatal(err)
		}
		fmt.Println("Registration token rotated. Clients must re-register. New token:")
		fmt.Printf("  %s\n", tok)
	default:
		fatalf("usage: menagerie-relay token [print|rotate]")
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `menagerie-relay — a Menagerie relay

Usage:
  menagerie-relay serve             start the relay (default). Creates the config on
                                    first run and copies the registration token to
                                    your clipboard — just paste it into Menagerie.
  menagerie-relay service install   run the relay always-on: starts at login and
                                    restarts if it exits (launchd on macOS,
                                    systemd --user on Linux)
  menagerie-relay service uninstall remove the always-on service
  menagerie-relay service status    is the always-on service running?
  menagerie-relay init              write the config + token only (no serve)
  menagerie-relay token print       re-print (and copy) the registration token
  menagerie-relay token rotate      generate a new registration token (invalidates the old)
`)
}

func fatal(err error) {
	log.Fatalf("error: %v", err)
}

func fatalf(format string, args ...any) {
	log.Fatalf("error: "+format, args...)
}
