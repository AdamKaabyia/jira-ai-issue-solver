package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"go.uber.org/zap"
)

// Config holds construction parameters for [Server].
type Config struct {
	// TargetURL is the upstream proxy base URL (scheme + host),
	// e.g. "https://models.corp.example.com".
	TargetURL string

	// AuthToken is the Bearer token sent to the upstream proxy.
	AuthToken string

	// RewritePath translates an incoming URL path (Vertex format)
	// to the upstream's expected format. See [VertexToModelsCorp].
	RewritePath func(path string) (string, error)

	// Logger is used for per-request debug logging.
	Logger *zap.Logger
}

// Server is an HTTP reverse proxy that rewrites URL paths and injects
// authentication headers before forwarding to an upstream API proxy.
// It is designed to bridge Claude Code CLI's Vertex AI URL format with
// non-standard upstream proxies (e.g. Models.corp).
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	logger     *zap.Logger
}

// NewServer creates a Server but does not start listening. Call
// [Server.Start] to begin accepting connections.
func NewServer(cfg Config) (*Server, error) {
	if cfg.TargetURL == "" {
		return nil, errors.New("proxy: target URL must not be empty")
	}
	if cfg.RewritePath == nil {
		return nil, errors.New("proxy: rewrite function must not be nil")
	}
	if cfg.Logger == nil {
		return nil, errors.New("proxy: logger must not be nil")
	}

	target, err := url.Parse(cfg.TargetURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse target URL: %w", err)
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			rewritten, rwErr := cfg.RewritePath(req.URL.Path)
			if rwErr != nil {
				cfg.Logger.Warn("Path rewrite failed",
					zap.String("path", req.URL.Path),
					zap.Error(rwErr))
				return
			}

			cfg.Logger.Debug("Proxying request",
				zap.String("original_path", req.URL.Path),
				zap.String("rewritten_path", rewritten))

			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = rewritten
			req.Host = target.Host

			if cfg.AuthToken != "" {
				req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
			}
		},
		FlushInterval: -1, // flush immediately for SSE streaming
	}

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("proxy: listen: %w", err)
	}

	return &Server{
		httpServer: &http.Server{Handler: rp},
		listener:   ln,
		logger:     cfg.Logger,
	}, nil
}

// Start begins serving HTTP requests. It returns immediately; the
// server runs in a background goroutine.
func (s *Server) Start() {
	go func() {
		if err := s.httpServer.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("Proxy server error", zap.Error(err))
		}
	}()
	s.logger.Info("URL translation proxy started",
		zap.String("addr", s.listener.Addr().String()))
}

// Addr returns the listener address in "host:port" format. The port
// is assigned by the OS when the listener is created.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Port returns just the port number the proxy is listening on.
func (s *Server) Port() string {
	_, port, _ := net.SplitHostPort(s.listener.Addr().String())
	return port
}

// Close gracefully shuts down the proxy server.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}
