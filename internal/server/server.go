package server

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"
)

type Server struct {
	httpServer *http.Server
}

func New(bindAddress string, port int, handler http.Handler) *Server {
	addr := net.JoinHostPort(bindAddress, strconv.Itoa(port))
	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

func (s *Server) Addr() string {
	return s.httpServer.Addr
}

func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
