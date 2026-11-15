// Command agent-room runs a chat room server where people and AI agents work
// together. Open the printed address, join, and use "Invite an agent".
package main

import (
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KakkoiDev/agent-room/room"
)

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:8790", "address to listen on")
	dir := flag.String("dir", filepath.Join(home, ".agent-room"), "folder for rooms, transcripts and sessions")
	base := flag.String("base", "", "path prefix when served under a sub-path, such as /agents")
	public := flag.String("public-url", "", "address people and agents use, such as https://box.example.ts.net (default: what the browser uses)")
	tailscale := flag.Bool("tailscale", false, "require the Tailscale-User-Login header set by Tailscale Serve")
	login := flag.String("tailscale-login", "", "allow only this Tailscale login (implies -tailscale)")
	var origins, hosts list
	flag.Var(&origins, "allow-origin", "web app origin that may use the API from the browser, such as https://app.example or http://localhost:* (repeatable, or comma-separated)")
	flag.Var(&hosts, "allow-host", "extra host name the server answers to, such as box.lan or *.example.com (repeatable, or comma-separated)")
	house := flag.String("house-agent", "", "name of an agent that considers every message that @mentions no one and answers only when needed")
	flag.Parse()

	store, err := room.NewFileStore(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	// Answer only to names that mean this server, so a web page cannot reach it
	// by DNS rebinding.
	hosts = append(hosts, "localhost", "127.0.0.1", "::1")
	if host, _, _ := net.SplitHostPort(*addr); host != "" && !net.ParseIP(host).IsUnspecified() {
		hosts = append(hosts, host)
	}
	if *public != "" {
		u, err := url.Parse(*public)
		if err != nil || u.Hostname() == "" {
			log.Fatalf("-public-url must be an address such as https://box.example.ts.net")
		}
		hosts = append(hosts, u.Hostname())
	}
	if *tailscale || *login != "" {
		hosts = append(hosts, "*.ts.net")
	}
	opts := room.Options{BasePath: *base, PublicURL: *public, AllowedOrigins: origins, AllowedHosts: hosts, HouseAgent: *house}
	if *tailscale || *login != "" {
		opts.Authorize = func(r *http.Request) error {
			got := r.Header.Get("Tailscale-User-Login")
			if got == "" || (*login != "" && got != *login) {
				return errors.New("Connect through Tailscale Serve with an authorized account.")
			}
			return nil
		}
	} else if host, _, _ := net.SplitHostPort(*addr); !isLoopback(host) {
		log.Printf("Warning: %s is reachable from other machines and anyone who can reach it can join. Use -tailscale or put it behind your own login.", *addr)
	}

	server := room.New(store, opts)
	handler := server.Handler()
	if *base != "" {
		mux := http.NewServeMux()
		mux.Handle(*base+"/", http.StripPrefix(*base, handler))
		handler = mux
	}
	log.Printf("Agent room: open http://%s%s/chat (data in %s)", *addr, *base, *dir)
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// list is a flag that may be repeated or given comma-separated values.
type list []string

func (l *list) String() string { return strings.Join(*l, ",") }

func (l *list) Set(v string) error {
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			*l = append(*l, item)
		}
	}
	return nil
}
