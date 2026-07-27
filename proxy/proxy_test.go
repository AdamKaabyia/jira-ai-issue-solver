package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

func TestServer_ProxiesAndRewritesPath(t *testing.T) {
	var mu sync.Mutex
	var receivedPath string
	var receivedAuth string
	var receivedBody string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	srv, err := NewServer(Config{
		TargetURL:   backend.URL,
		AuthToken:   "test-token-123",
		RewritePath: VertexToModelsCorp,
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Start()
	defer func() { _ = srv.Close() }()

	reqBody := `{"anthropic_version":"vertex-2023-10-16","messages":[{"role":"user","content":"hi"}]}`
	vertexPath := "/projects/dummy/locations/us-east1/publishers/anthropic/models/claude-sonnet-4-6:streamRawPredict"

	resp, err := http.Post(
		"http://"+srv.Addr()+vertexPath,
		"application/json",
		strings.NewReader(reqBody),
	)
	if err != nil {
		t.Fatalf("POST to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()

	wantPath := "/sonnet/models/claude-sonnet-4-6:streamRawPredict"
	if receivedPath != wantPath {
		t.Errorf("backend received path = %q, want %q", receivedPath, wantPath)
	}
	if receivedAuth != "Bearer test-token-123" {
		t.Errorf("backend received auth = %q, want %q", receivedAuth, "Bearer test-token-123")
	}
	if receivedBody != reqBody {
		t.Errorf("backend received body = %q, want %q", receivedBody, reqBody)
	}
}

func TestServer_StreamingPassthrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("backend response writer does not implement Flusher")
		}
		for _, event := range []string{
			"data: {\"type\":\"content_block_start\"}\n\n",
			"data: {\"type\":\"content_block_delta\"}\n\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = w.Write([]byte(event))
			flusher.Flush()
		}
	}))
	defer backend.Close()

	srv, err := NewServer(Config{
		TargetURL:   backend.URL,
		AuthToken:   "tok",
		RewritePath: VertexToModelsCorp,
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Start()
	defer func() { _ = srv.Close() }()

	resp, err := http.Post(
		"http://"+srv.Addr()+"/projects/p/locations/r/publishers/anthropic/models/claude-sonnet-4-6:streamRawPredict",
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST to proxy: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "content_block_start") {
		t.Errorf("streaming body missing expected event, got: %s", body)
	}
	if !strings.Contains(string(body), "message_stop") {
		t.Errorf("streaming body missing final event, got: %s", body)
	}
}

func TestServer_Port(t *testing.T) {
	srv, err := NewServer(Config{
		TargetURL:   "https://example.com",
		AuthToken:   "tok",
		RewritePath: VertexToModelsCorp,
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Start()
	defer func() { _ = srv.Close() }()

	port := srv.Port()
	if port == "" || port == "0" {
		t.Errorf("Port() = %q, want a non-zero port", port)
	}
}

func TestNewServer_ValidationErrors(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "empty target URL",
			cfg:  Config{TargetURL: "", RewritePath: VertexToModelsCorp, Logger: logger},
		},
		{
			name: "nil rewrite function",
			cfg:  Config{TargetURL: "https://example.com", RewritePath: nil, Logger: logger},
		},
		{
			name: "nil logger",
			cfg:  Config{TargetURL: "https://example.com", RewritePath: VertexToModelsCorp, Logger: nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewServer(tt.cfg)
			if err == nil {
				t.Error("NewServer() should have returned an error")
			}
		})
	}
}

func TestServer_NoAuthToken(t *testing.T) {
	var receivedAuth string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv, err := NewServer(Config{
		TargetURL:   backend.URL,
		AuthToken:   "",
		RewritePath: VertexToModelsCorp,
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Start()
	defer func() { _ = srv.Close() }()

	resp, err := http.Post(
		"http://"+srv.Addr()+"/projects/p/locations/r/publishers/anthropic/models/claude-sonnet-4-6:rawPredict",
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if receivedAuth != "" {
		t.Errorf("auth header = %q, want empty (no token configured)", receivedAuth)
	}
}
