// Command agent-room runs a chat room server where people and AI agents work
// together. Open the printed address, join, and use "Invite an agent".
package main

import (
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	flag.Parse()

	store, err := room.NewFileStore(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	opts := room.Options{BasePath: *base, PublicURL: *public}
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
