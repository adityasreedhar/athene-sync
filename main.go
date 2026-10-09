// Athene sync server: keeps Athene and KOReader in step through a server the reader runs.
//
// It speaks KOReader's Progress sync API exactly (/users/create, /users/auth, /syncs/progress,
// /healthcheck), so KOReader's own Progress sync works with it unchanged, and adds what full sync
// needs under /athene/v1: each book's highlights, notes and bookmarks, the books Athene sends to
// the e-reader, and the KOReader plugin that does the e-reader's side.
//
// Everything lives in one data folder: a small JSON file per account and per book, and the books
// themselves. Nothing leaves the server.
//
// Copyright (C) 2026 Aditya Sreedhar. Free software under the GNU Affero General Public License,
// version 3 (LICENSE); the source is served at /source.
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
)

// Version is the server's; the plugin carries its own in plugin/athene.koplugin/_meta.lua.
const Version = "1.0.3"

// APIVersion is what /athene/v1/info reports, for clients to check what they can ask for.
const APIVersion = 1

func main() {
	listen := flag.String("listen", env("ATHENE_LISTEN", ":17200"), "address to listen on")
	data := flag.String("data", env("ATHENE_DATA", "/data"), "folder for accounts, books and annotations")
	health := flag.Bool("healthcheck", false, "ask a running server whether it is up, and exit (for Docker's HEALTHCHECK)")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *version {
		fmt.Println("athene-sync", Version)
		return
	}
	if *health {
		os.Exit(healthcheck(*listen))
	}

	store, err := OpenStore(*data)
	if err != nil {
		log.Fatalf("athene-sync: %v", err)
	}
	srv := NewServer(store, Options{
		Registration: envBool("ATHENE_REGISTRATION", true),
		MaxBookBytes: int64(envInt("ATHENE_MAX_BOOK_MB", 512)) << 20,
	})

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.New(os.Stderr, "athene-sync: ", log.LstdFlags),
	}

	stopped := make(chan struct{})
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		<-signals
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
		close(stopped)
	}()

	registration := "open"
	if !srv.opts.Registration {
		registration = "closed"
	}
	log.Printf("athene-sync %s listening on %s, data in %s, registration %s", Version, *listen, *data, registration)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("athene-sync: %v", err)
	}
	<-stopped
	log.Printf("athene-sync stopped")
}

// healthcheck asks the server on this machine for /healthcheck, as KOReader would.
func healthcheck(listen string) int {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthcheck", nil)
	if err != nil {
		return 1
	}
	req.Header.Set("Accept", "application/vnd.koreader.v1+json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return fallback
}
