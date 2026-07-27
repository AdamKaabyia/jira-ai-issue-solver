// Package proxy provides an HTTP reverse proxy that translates
// Claude Code CLI's standard Vertex AI URL paths to the custom paths
// used by non-standard API proxies such as Red Hat's Models.corp.
package proxy

import (
	"fmt"
	"regexp"
	"strings"
)

// vertexPathRe matches the Vertex AI URL path that Claude Code CLI
// constructs when CLAUDE_CODE_USE_VERTEX=1:
//
//	/projects/{project}/locations/{location}/publishers/anthropic/models/{model}:{action}
//
// The capture group extracts the "{model}:{action}" suffix.
var vertexPathRe = regexp.MustCompile(
	`^/projects/[^/]+/locations/[^/]+/publishers/anthropic/models/(.+)$`,
)

// knownFamilies maps model-name substrings to the Models.corp path
// prefix. Order matters: the first match wins.
var knownFamilies = []struct {
	keyword string
	family  string
}{
	{"sonnet", "sonnet"},
	{"haiku", "haiku"},
	{"opus", "opus"},
}

// VertexToModelsCorp rewrites a Vertex AI URL path to the
// Models.corp path format.
//
// Input (from Claude Code):
//
//	/projects/dummy/locations/us-east1/publishers/anthropic/models/claude-sonnet-4-6:streamRawPredict
//
// Output:
//
//	/sonnet/models/claude-sonnet-4-6:streamRawPredict
func VertexToModelsCorp(path string) (string, error) {
	matches := vertexPathRe.FindStringSubmatch(path)
	if matches == nil {
		return "", fmt.Errorf("path does not match Vertex AI format: %s", path)
	}

	modelAndAction := matches[1] // e.g. "claude-sonnet-4-6:streamRawPredict"

	// Split model from action at the first colon.
	model := modelAndAction
	if idx := strings.Index(modelAndAction, ":"); idx > 0 {
		model = modelAndAction[:idx]
	}

	family, err := modelFamily(model)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("/%s/models/%s", family, modelAndAction), nil
}

// modelFamily derives the Models.corp path prefix from a Claude model
// name (e.g. "claude-sonnet-4-6" → "sonnet").
func modelFamily(model string) (string, error) {
	lower := strings.ToLower(model)
	for _, f := range knownFamilies {
		if strings.Contains(lower, f.keyword) {
			return f.family, nil
		}
	}
	return "", fmt.Errorf("unknown model family for %q: expected name containing sonnet, haiku, or opus", model)
}
