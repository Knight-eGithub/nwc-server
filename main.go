package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type Server struct {
	pool    *WorkerPool
	isolate *IsolateRunner
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	server := &Server{
		pool:    NewWorkerPool(2), // Max 2 concurrent executions
		isolate: NewIsolateRunner(),
	}

	// Purge any stale boxes or dangling mounts from previous runs on boot
	for i := 0; i <= 2; i++ {
		_ = server.isolate.CleanupBox(i)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", withCORS(server.handleHealth))
	mux.HandleFunc("/execute", withCORS(server.handleExecute))

	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Handle graceful shutdown on Ctrl+C / SIGTERM
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("[nwc-server] Shutting down gracefully...")
		_ = httpServer.Close()
	}()

	log.Printf("[nwc-server] High-performance execution engine listening on :%s (Concurrency Cap: 2 slots)\n", port)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[nwc-server] Server failed: %v\n", err)
	}
}

// withCORS middleware enables direct client-side fallback requests without browser CORS errors
func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-client-id, x-run-id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

// GET /health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"concurrency_cap": 2,
		"engine":          "go-isolate",
	})
}

// POST /execute
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
		return
	}

	if req.Code == "" {
		http.Error(w, "code cannot be empty", http.StatusBadRequest)
		return
	}

	// 1. Acquire an execution slot (waits up to 15s in memory queue)
	ctx := r.Context()
	slotID, err := s.pool.Acquire(ctx, 15*time.Second)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		json.NewEncoder(w).Encode(ExecuteResponse{
			Status: "timeout",
			Stderr: "Server busy: Execution queue timed out. Please try again in a moment.",
		})
		return
	}
	defer s.pool.Release(slotID) // Release slot back to pool when done

	// 2. Dispatch to the appropriate language runner
	var resp *ExecuteResponse
        switch req.Language {
        case "cpp", "c++":
                resp, err = RunCpp(ctx, s.isolate, slotID, req)
        case "java":
                resp, err = RunJava(ctx, s.isolate, slotID, req)
        case "python", "py", "python3":
                resp, err = RunPython(ctx, s.isolate, slotID, req)
        case "javascript", "js", "node":
                resp, err = RunJavaScript(ctx, s.isolate, slotID, req)
        default:
                w.Header().Set("Content-Type", "application/json")
                w.WriteHeader(http.StatusBadRequest)
                json.NewEncoder(w).Encode(ExecuteResponse{
                        Status: "error",
                        Stderr: fmt.Sprintf("Unsupported server-side language: %s", req.Language),
                })
                return
        }
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ExecuteResponse{
			Status: "error",
			Stderr: fmt.Sprintf("Engine execution error: %v", err),
		})
		return
	}

	// 3. Return JSON execution result
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
