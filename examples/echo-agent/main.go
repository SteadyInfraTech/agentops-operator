/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command echo-agent is a minimal reference agent that implements the
// AgentOps container contract:
//
//   - GET  /healthz  liveness (process is alive)
//   - GET  /readyz   readiness (ready to accept sessions)
//   - GET  /metrics  Prometheus metrics
//   - POST /v1/chat  {"message": "..."} -> {"reply": "..."}
//
// Configuration is read from the AGENT_* environment variables injected by
// the operator. It echoes input instead of calling a model so the operator
// can be exercised without LLM credentials.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const maxBodyBytes = 64 << 10

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_requests_total", Help: "Chat requests by result.",
	}, []string{"code"})
	latency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "agent_request_duration_seconds", Help: "Chat request latency.", Buckets: prometheus.DefBuckets,
	})
)

type chatRequest struct {
	Message string `json:"message"`
}

type chatResponse struct {
	Agent string `json:"agent"`
	Model string `json:"model,omitempty"`
	Reply string `json:"reply"`
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	addr := ":" + envOr("PORT", "8080")
	name := envOr("AGENT_NAME", "echo-agent")
	model := os.Getenv("AGENT_MODEL_NAME")

	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /v1/chat", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() { latency.Observe(time.Since(start).Seconds()) }()

		var req chatRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || req.Message == "" {
			requests.WithLabelValues("400").Inc()
			http.Error(w, `expected JSON body {"message": "..."}`, http.StatusBadRequest)
			return
		}
		requests.WithLabelValues("200").Inc()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{Agent: name, Model: model, Reply: "echo: " + req.Message})
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("starting agent", "addr", addr, "agent", name, "model", model)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	ready.Store(true)

	<-ctx.Done()
	// Fail readiness first so the Service stops routing new traffic, then drain.
	ready.Store(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
