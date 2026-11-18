package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Global server state.
var (
	store = NewStore()
	token string // the bearer token every API call must carry
)

// jsonWrite writes a JSON body with the request-id header.
func jsonWrite(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", "req-"+requestID())
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// errJSON writes a JSON error body {message: ...}.
func errJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", "req-"+requestID())
	body, _ := json.Marshal(map[string]string{"message": msg})
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

var reqSeq uint64

func requestID() string {
	reqSeq++
	return fmt.Sprintf("%d", reqSeq)
}

// authed reports whether the request carries the trial's bearer token.
func authed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return strings.TrimSpace(h[len("Bearer "):]) == token
}

// requireAuth sends 401 when the token is missing/invalid; returns true when ok.
func requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if authed(r) {
		return true
	}
	errJSON(w, 401, "Bad credentials")
	return false
}

func readJSONBody(r *http.Request, dst interface{}) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("empty body")
	}
	return json.Unmarshal(body, dst)
}

func main() {
	token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	envURL := os.Getenv("MAKEFUN_ENV_URL")

	mux := http.NewServeMux()
	registerHTTP(mux)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()

	if envURL == "" {
		// Practice mode: serve until killed (the sdk/run.ts iteration loop).
		fmt.Printf("listening on http://127.0.0.1:%d\n", port)
		select {}
	}

	// Lifecycle: register then poll /done.
	regBody, _ := json.Marshal(map[string]int{"port": port})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(strings.TrimRight(envURL, "/")+"/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	doneURL := strings.TrimRight(envURL, "/") + "/done"
	attempt := 0
	for {
		time.Sleep(donePollDelayMs(attempt))
		attempt++
		resp, err := client.Get(doneURL)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var done struct {
			Done bool `json:"done"`
		}
		if json.Unmarshal(body, &done) == nil && done.Done {
			os.Exit(0)
		}
	}
}

// donePollDelayMs mirrors wire.ts donePollDelayMs: 250ms doubling to 2000ms cap.
func donePollDelayMs(attempt int) time.Duration {
	first := 250 * time.Millisecond
	cap := 2000 * time.Millisecond
	d := first << uint(attempt)
	if d < first {
		d = first
	}
	if d > cap {
		d = cap
	}
	return d
}

func registerHTTP(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/graphql", serveGraphQL)
	mux.HandleFunc("/mcp", serveMCP)
	mux.HandleFunc("/api/v3/", serveREST)
	mux.HandleFunc("/api/v3", serveREST)
	// git smart HTTP: /{owner}/{repo}.git/...
	mux.HandleFunc("/", serveGitOr404)
}

// serveGitOr404 dispatches /{owner}/{repo}.git/info/refs and the POST endpoints.
func serveGitOr404(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !strings.HasSuffix(p, ".git") && !strings.Contains(p, ".git/") {
		// A REST route that wasn't under /api/v3? JSON 404 for the protocol checks.
		if strings.HasPrefix(p, "/api/") {
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		http.NotFound(w, r)
		return
	}
	// Parse owner/repo from /{owner}/{repo}.git
	rest := p
	dotIdx := strings.Index(rest, ".git")
	if dotIdx < 0 {
		http.NotFound(w, r)
		return
	}
	prefix := rest[:dotIdx]
	suffix := rest[dotIdx+len(".git"):] // "/info/refs" or "/git-upload-pack" or ""
	parts := strings.SplitN(strings.TrimPrefix(prefix, "/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	owner, repo := parts[0], parts[1]

	switch suffix {
	case "/info/refs":
		service := r.URL.Query().Get("service")
		store.serveInfoRefs(w, r, owner, repo, service)
	case "/git-upload-pack":
		store.serveUploadPack(w, r, owner, repo)
	case "/git-receive-pack":
		store.serveReceivePack(w, r, owner, repo)
	default:
		http.NotFound(w, r)
	}
}
