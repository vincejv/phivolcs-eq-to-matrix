package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// ---- Health check state ----
var (
	healthMu     sync.RWMutex
	lastFetchOK  bool
	lastFetchAt  time.Time
	lastFetchErr string
)

// startHealthServer starts a small HTTP server exposing /health (and /healthz)
// so external monitors/orchestrators (e.g. Docker, Kubernetes) can probe liveness.
func startHealthServer() {
	port := os.Getenv("HEALTH_PORT")
	if port == "" {
		port = DEFAULT_HEALTH_PORT
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", healthHandler)

	go func() {
		addr := ":" + port
		log.Printf("🏥 Health check listening on %s (GET /health)", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("❌ Health server failed: %v", err)
		}
	}()
}

// setHealth records the outcome of the most recent PHIVOLCS fetch attempt.
// ok=true means the fetch succeeded; ok=false records the error for reporting.
func setHealth(ok bool, err error) {
	healthMu.Lock()
	defer healthMu.Unlock()
	lastFetchOK = ok
	lastFetchAt = time.Now()
	if err != nil {
		lastFetchErr = err.Error()
	} else {
		lastFetchErr = ""
	}
}

// healthHandler reports whether the last PHIVOLCS fetch succeeded.
// Returns 200 OK if the last fetch was successful, 503 Service Unavailable otherwise
// (including the case where no fetch has completed yet).
func healthHandler(w http.ResponseWriter, r *http.Request) {
	healthMu.RLock()
	ok := lastFetchOK
	at := lastFetchAt
	errMsg := lastFetchErr
	healthMu.RUnlock()

	resp := map[string]interface{}{
		"status": "ok",
	}
	if at.IsZero() {
		resp["status"] = "unknown"
		resp["message"] = "no fetch attempted yet"
	} else {
		resp["lastFetchAt"] = at.Format(time.RFC3339)
		if !ok {
			resp["status"] = "error"
			resp["error"] = errMsg
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if ok {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
}
