// retraction-checker-web is a thin HTTP wrapper around the keyless
// retraction-checker CLI. Each API endpoint shells out to the compiled CLI
// binary with --json and forwards the parsed JSON to the client.
//
// No API keys are involved: the underlying CLI hits Crossref, OpenAlex, and
// PubMed, all of which are open/keyless.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// cliTimeout bounds each shell-out to the CLI. Live upstream calls (OpenAlex in
// particular) can be slow, so give them room.
const cliTimeout = 60 * time.Second

// cliBinary resolves the path to the compiled CLI binary. Override with the
// CLI_BIN env var; otherwise default to ./bin/<name> with a platform-correct
// extension so the same code works on Windows (dev) and Linux (Render).
func cliBinary() string {
	if p := os.Getenv("CLI_BIN"); p != "" {
		return p
	}
	name := "retraction-checker-pp-cli"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join("bin", name)
}

// runCLI executes the CLI with the given args, returning its stdout. stdout is
// expected to be JSON; stderr is captured separately (the CLI emits warnings
// there that must not corrupt the JSON body) and surfaced only on failure.
func runCLI(ctx context.Context, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cliBinary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}

	out := bytes.TrimSpace(stdout.Bytes())
	if !json.Valid(out) {
		detail := stderr.String()
		if detail == "" {
			detail = string(out)
		}
		return nil, errors.New("CLI did not return valid JSON: " + detail)
	}
	return json.RawMessage(out), nil
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a uniform {"error": "..."} body.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// forwardCLI runs the CLI and writes its JSON straight through to the client,
// or a 502 with the CLI's error text on failure.
func forwardCLI(w http.ResponseWriter, r *http.Request, args ...string) {
	raw, err := runCLI(r.Context(), args...)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// decodeBody parses the request's JSON body into dst. Empty bodies are allowed
// (dst keeps its zero values) so callers can supply optional fields only.
func decodeBody(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return err
		}
		return err
	}
	return nil
}

// POST /api/check {"doi": "..."} -> check <doi> --json
func handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		DOI string `json:"doi"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.DOI == "" {
		writeError(w, http.StatusBadRequest, "field 'doi' is required")
		return
	}
	forwardCLI(w, r, "check", body.DOI, "--json")
}

// POST /api/search {"query": "...", "limit": N} -> works search --query <q> --rows <n> --json
func handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.Query == "" {
		writeError(w, http.StatusBadRequest, "field 'query' is required")
		return
	}
	limit := body.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}
	forwardCLI(w, r, "works", "search", "--query", body.Query, "--rows", strconv.Itoa(limit), "--json")
}

// POST /api/superseded {"doi": "...", "limit": N} -> superseded <doi> --limit <n> --json
func handleSuperseded(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		DOI   string `json:"doi"`
		Limit int    `json:"limit"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.DOI == "" {
		writeError(w, http.StatusBadRequest, "field 'doi' is required")
		return
	}
	args := []string{"superseded", body.DOI, "--json"}
	if body.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(body.Limit))
	}
	forwardCLI(w, r, args...)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// handleRoot serves the single-file frontend.
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "index.html")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8092"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/check", handleCheck)
	mux.HandleFunc("/api/search", handleSearch)
	mux.HandleFunc("/api/superseded", handleSuperseded)
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/", handleRoot)

	srv := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("retraction-checker-web listening on 0.0.0.0:%s (CLI=%s)", port, cliBinary())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
