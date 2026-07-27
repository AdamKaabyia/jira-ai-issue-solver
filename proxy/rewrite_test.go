package proxy

import (
	"testing"
)

func TestVertexToModelsCorp(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{
			name: "sonnet streamRawPredict",
			path: "/projects/dummy/locations/us-east1/publishers/anthropic/models/claude-sonnet-4-6:streamRawPredict",
			want: "/sonnet/models/claude-sonnet-4-6:streamRawPredict",
		},
		{
			name: "sonnet rawPredict",
			path: "/projects/myproj/locations/global/publishers/anthropic/models/claude-sonnet-4-6:rawPredict",
			want: "/sonnet/models/claude-sonnet-4-6:rawPredict",
		},
		{
			name: "haiku streamRawPredict",
			path: "/projects/p/locations/us-east1/publishers/anthropic/models/claude-haiku-4-5:streamRawPredict",
			want: "/haiku/models/claude-haiku-4-5:streamRawPredict",
		},
		{
			name: "opus streamRawPredict",
			path: "/projects/p/locations/eu/publishers/anthropic/models/claude-opus-4-6:streamRawPredict",
			want: "/opus/models/claude-opus-4-6:streamRawPredict",
		},
		{
			name: "versioned model name",
			path: "/projects/p/locations/us/publishers/anthropic/models/claude-sonnet-4-5-v2@20250514:streamRawPredict",
			want: "/sonnet/models/claude-sonnet-4-5-v2@20250514:streamRawPredict",
		},
		{
			name: "case-insensitive family",
			path: "/projects/p/locations/r/publishers/anthropic/models/Claude-Sonnet-4-6:rawPredict",
			want: "/sonnet/models/Claude-Sonnet-4-6:rawPredict",
		},
		{
			name:    "unknown model family",
			path:    "/projects/p/locations/r/publishers/anthropic/models/claude-unknown-9:streamRawPredict",
			wantErr: true,
		},
		{
			name:    "not a vertex path",
			path:    "/v1/messages",
			wantErr: true,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
		{
			name:    "missing model segment",
			path:    "/projects/p/locations/r/publishers/anthropic/models/",
			wantErr: true,
		},
		{
			name: "token counting path",
			path: "/projects/p/locations/r/publishers/anthropic/models/claude-sonnet-4-6:count-tokens",
			want: "/sonnet/models/claude-sonnet-4-6:count-tokens",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := VertexToModelsCorp(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Errorf("VertexToModelsCorp(%q) = %q, want error", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("VertexToModelsCorp(%q) unexpected error: %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("VertexToModelsCorp(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestModelFamily(t *testing.T) {
	tests := []struct {
		model   string
		want    string
		wantErr bool
	}{
		{"claude-sonnet-4-6", "sonnet", false},
		{"claude-haiku-4-5", "haiku", false},
		{"claude-opus-4-6", "opus", false},
		{"Claude-Sonnet-4-6", "sonnet", false},
		{"totally-unknown-model", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, err := modelFamily(tt.model)
			if tt.wantErr {
				if err == nil {
					t.Errorf("modelFamily(%q) = %q, want error", tt.model, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("modelFamily(%q) unexpected error: %v", tt.model, err)
			}
			if got != tt.want {
				t.Errorf("modelFamily(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}
