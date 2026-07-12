package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

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

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "index.html")
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func cliBinary() string {
	if b := os.Getenv("CLI_BIN"); b != "" {
		return b
	}
	return "./retraction-checker"
}

// ------ API: /api/check ------
type checkRequest struct {
	DOI string `json:"doi"`
}

type checkResponse struct {
	Retracted  bool   `json:"retracted"`
	UpdateType string `json:"update_type,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Notice     string `json:"notice,omitempty"`
	Title      string `json:"title,omitempty"`
}

type cliCheckResponse struct {
	Retracted  bool   `json:"retracted"`
	UpdateType string `json:"update_type"`
	Reason     string `json:"reason"`
	Notice     string `json:"notice"`
	Title      string `json:"title"`
}

func handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST", http.StatusMethodNotAllowed)
		return
	}
	var req checkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.DOI == "" {
		http.Error(w, "missing doi", http.StatusBadRequest)
		return
	}

	bin := cliBinary()
	cmd := exec.Command(bin, "check", req.DOI, "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		errMsg := fmt.Sprintf("CLI error: %v, stderr: %s", err, stderr.String())
		log.Print(errMsg)
		http.Error(w, errMsg, http.StatusInternalServerError)
		return
	}

	var cliResp cliCheckResponse
	if err := json.Unmarshal(stdout.Bytes(), &cliResp); err != nil {
		// fallback: parse text lines
		out := stdout.String()
		resp := checkResponse{Retracted: false}
		lines := strings.Split(out, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "retracted:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "retracted:"))
				resp.Retracted = val == "true"
			} else if strings.HasPrefix(line, "update_type:") {
				resp.UpdateType = strings.TrimSpace(strings.TrimPrefix(line, "update_type:"))
			} else if strings.HasPrefix(line, "reason:") {
				resp.Reason = strings.TrimSpace(strings.TrimPrefix(line, "reason:"))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	resp := checkResponse{
		Retracted:  cliResp.Retracted,
		UpdateType: cliResp.UpdateType,
		Reason:     cliResp.Reason,
		Notice:     cliResp.Notice,
		Title:      cliResp.Title,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ------ API: /api/search ------
type searchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST", http.StatusMethodNotAllowed)
		return
	}
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Query == "" {
		http.Error(w, "missing query", http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	bin := cliBinary()
	args := []string{"search", req.Query, "--limit", fmt.Sprintf("%d", req.Limit), "--json"}
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		errMsg := fmt.Sprintf("CLI search error: %v, stderr: %s", err, stderr.String())
		log.Print(errMsg)
		http.Error(w, errMsg, http.StatusInternalServerError)
		return
	}

	// A CLI kimenete JSON
	var cliOutput map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &cliOutput); err != nil {
		http.Error(w, "CLI output is not valid JSON", http.StatusInternalServerError)
		return
	}

	// Átalakítás a frontend által várt formátumra:
	// { "results": { "message": { "items": [...] } } }
	var items []interface{}

	// Próbáljuk kiszedni a találatokat a CLI JSON-jából
	if res, ok := cliOutput["results"].([]interface{}); ok {
		items = res
	} else if res, ok := cliOutput["items"].([]interface{}); ok {
		items = res
	} else if res, ok := cliOutput["message"].(map[string]interface{}); ok {
		if itemsArr, ok := res["items"].([]interface{}); ok {
			items = itemsArr
		}
	} else {
		// Ha egyéb, próbáljuk az egészet egy tömbben visszaadni
		items = []interface{}{cliOutput}
	}

	response := map[string]interface{}{
		"results": map[string]interface{}{
			"message": map[string]interface{}{
				"items": items,
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// ------ API: /api/superseded ------
type supersededRequest struct {
	DOI string `json:"doi"`
}

type supersededResponse struct {
	Superseded bool     `json:"superseded"`
	Reason     string   `json:"reason,omitempty"`
	Results    []string `json:"results,omitempty"`
}

type cliSupersededResponse struct {
	Results []struct {
		Title string `json:"title"`
		DOI   string `json:"doi"`
	} `json:"results"`
}

func handleSuperseded(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST", http.StatusMethodNotAllowed)
		return
	}
	var req supersededRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.DOI == "" {
		http.Error(w, "missing doi", http.StatusBadRequest)
		return
	}

	bin := cliBinary()
	cmd := exec.Command(bin, "superseded", req.DOI, "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		errMsg := fmt.Sprintf("CLI superseded error: %v, stderr: %s", err, stderr.String())
		log.Print(errMsg)
		http.Error(w, errMsg, http.StatusInternalServerError)
		return
	}
	var cliResp cliSupersededResponse
	if err := json.Unmarshal(stdout.Bytes(), &cliResp); err != nil {
		resp := supersededResponse{Superseded: false, Results: []string{stdout.String()}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}
	results := make([]string, len(cliResp.Results))
	for i, item := range cliResp.Results {
		results[i] = fmt.Sprintf("%s (%s)", item.Title, item.DOI)
	}
	resp := supersededResponse{Superseded: true, Results: results}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}