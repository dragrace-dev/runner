package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"dragrace/internal/lifecycle"
)

type Server struct {
	addr  string
	state *lifecycle.State
	http  *http.Server
}

func NewServer(addr string, state *lifecycle.State) *Server {
	server := &Server{addr: addr, state: state}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealth)
	server.http = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	return server
}

func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	go func() {
		_ = s.http.Serve(listener)
	}()
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	snapshot := s.state.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	if !snapshot.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(snapshot)
}
