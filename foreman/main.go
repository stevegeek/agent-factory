// Foreman: the maintainer's web app for an agent factory. It reads the factory's ticket files
// and shows what needs a person; every answer runs the factory's own CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

// list: a repeatable string flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	home, _ := os.UserHomeDir()
	root := flag.String("root", envOr("FACTORY_ROOT", filepath.Join(home, "factory")), "factory root (FACTORY_ROOT)")
	factory := flag.String("factory", "", "factory command (default <root>/bin/factory)")
	listen := flag.String("listen", "127.0.0.1:8790", "address to listen on")
	secretFile := flag.String("secret-file", filepath.Join(home, ".config", "factory", "foreman-secret"), "login secret; created when missing")
	trustTS := flag.Bool("trust-tailscale", false, "accept Tailscale-User-Login from loopback (tailscale serve) for the --tailscale-user logins")
	var tsUsers, hosts, projects list
	flag.Var(&tsUsers, "tailscale-user", "a Tailscale login that counts as the maintainer (repeatable; needed with --trust-tailscale)")
	flag.Var(&projects, "project", "a project a new ticket may name (repeatable; deploy passes [projects])")
	flag.Var(&hosts, "host", "a host name to answer to besides IP addresses and localhost, e.g. the ts.net name (repeatable)")
	revoke := flag.Bool("revoke-sessions", false, "log out every browser, then exit")
	push := flag.Bool("push", true, "send Web Push notifications to subscribed devices (keys beside the secret)")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	if *revoke {
		if err := RevokeSessions(*secretFile); err != nil {
			log.Fatal(err)
		}
		fmt.Println("every session is revoked; log in again")
		return
	}
	if *trustTS && len(tsUsers) == 0 {
		log.Fatal("--trust-tailscale needs at least one --tailscale-user: the login(s) let in")
	}
	if *factory == "" {
		*factory = filepath.Join(*root, "bin", "factory")
	}
	// a small box: keep the heap tight rather than fast, and hand freed pages back between visits
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(32 << 20)
	go func() {
		for range time.Tick(time.Minute) {
			debug.FreeOSMemory()
		}
	}()

	secret, err := LoadSecret(*secretFile)
	if err != nil {
		log.Fatalf("secret: %v", err)
	}
	auth := NewAuth(secret)
	auth.since = sinceFile(*secretFile)
	auth.AllowHosts(hosts...)
	if *trustTS {
		auth.TrustTailscale(tsUsers...)
		log.Printf("trusting Tailscale-User-Login %s from loopback: any local process can send it", tsUsers.String())
	}
	if h, _, err := net.SplitHostPort(*listen); err == nil && !isLoopback(h) && h != "localhost" {
		log.Printf("WARNING: listening on %s over plain HTTP: the secret and the session cookie cross the network "+
			"in the clear. Prefer loopback behind `tailscale serve` or another TLS proxy.", *listen)
	}
	board := NewBoard(*root)
	s := NewServer(board, auth, &Runner{Factory: *factory, Root: *root})
	s.projects = projects
	if *push {
		// the VAPID contact: the page's own address when known (push services want an https: or mailto: URL)
		subject := "mailto:foreman@localhost"
		if len(hosts) > 0 {
			subject = "https://" + hosts[0]
		} else {
			log.Printf("push: no --host given; Apple's push service may refuse the placeholder contact %s", subject)
		}
		p, err := NewPusher(filepath.Dir(*secretFile), subject)
		if err != nil {
			log.Fatalf("push: %v", err)
		}
		s.push = p
		go p.Watch(board, 30*time.Second, nil)
		log.Printf("notifications on: %d device(s)", p.Count())
	}
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 11 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	log.Printf("foreman on http://%s (board %s, factory %s)", *listen, *root, *factory)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
