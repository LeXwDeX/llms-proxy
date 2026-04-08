package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

// ---------- extractStreamField ----------

func TestExtractStreamField(t *testing.T) {
	tests := []struct {
		name       string
		body       []byte
		wantStream bool
		wantFound  bool
	}{
		{
			name:       "stream true",
			body:       []byte(`{"model":"gpt-4","stream":true}`),
			wantStream: true,
			wantFound:  true,
		},
		{
			name:       "stream false",
			body:       []byte(`{"model":"gpt-4","stream":false}`),
			wantStream: false,
			wantFound:  true,
		},
		{
			name:       "no stream field",
			body:       []byte(`{"model":"gpt-4","messages":[]}`),
			wantStream: false,
			wantFound:  false,
		},
		{
			name:       "stream is string not bool",
			body:       []byte(`{"model":"gpt-4","stream":"yes"}`),
			wantStream: false,
			wantFound:  false,
		},
		{
			name:       "non-JSON body",
			body:       []byte(`this is not json`),
			wantStream: false,
			wantFound:  false,
		},
		{
			name:       "empty body",
			body:       nil,
			wantStream: false,
			wantFound:  false,
		},
		{
			name:       "empty object",
			body:       []byte(`{}`),
			wantStream: false,
			wantFound:  false,
		},
		{
			name:       "stream is number",
			body:       []byte(`{"stream":1}`),
			wantStream: false,
			wantFound:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStream, gotFound := extractStreamField(tt.body)
			if gotStream != tt.wantStream || gotFound != tt.wantFound {
				t.Errorf("extractStreamField(%q) = (%v, %v), want (%v, %v)",
					string(tt.body), gotStream, gotFound, tt.wantStream, tt.wantFound)
			}
		})
	}
}

// ---------- shouldAggregateSSE ----------

func TestShouldAggregateSSE(t *testing.T) {
	tests := []struct {
		name    string
		target  *Target
		reqBody []byte
		respCT  string
		want    bool
	}{
		{
			name:    "aggregate: non-streaming request, SSE response, auto-aggregate enabled",
			target:  &Target{SSEAutoAggregate: true},
			reqBody: []byte(`{"model":"gpt-4","stream":false}`),
			respCT:  "text/event-stream",
			want:    true,
		},
		{
			name:    "aggregate: no stream field, SSE response",
			target:  &Target{SSEAutoAggregate: true},
			reqBody: []byte(`{"model":"gpt-4"}`),
			respCT:  "text/event-stream; charset=utf-8",
			want:    true,
		},
		{
			name:    "no aggregate: client requested streaming",
			target:  &Target{SSEAutoAggregate: true},
			reqBody: []byte(`{"model":"gpt-4","stream":true}`),
			respCT:  "text/event-stream",
			want:    false,
		},
		{
			name:    "no aggregate: SSEAutoAggregate disabled",
			target:  &Target{SSEAutoAggregate: false},
			reqBody: []byte(`{"model":"gpt-4","stream":false}`),
			respCT:  "text/event-stream",
			want:    false,
		},
		{
			name:    "no aggregate: response is JSON not SSE",
			target:  &Target{SSEAutoAggregate: true},
			reqBody: []byte(`{"model":"gpt-4","stream":false}`),
			respCT:  "application/json",
			want:    false,
		},
		{
			name:    "no aggregate: nil target",
			target:  nil,
			reqBody: []byte(`{"model":"gpt-4"}`),
			respCT:  "text/event-stream",
			want:    false,
		},
		{
			name:    "aggregate: empty body, SSE response",
			target:  &Target{SSEAutoAggregate: true},
			reqBody: nil,
			respCT:  "text/event-stream",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldAggregateSSE(tt.target, tt.reqBody, tt.respCT)
			if got != tt.want {
				t.Errorf("shouldAggregateSSE() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------- aggregateSSEResponse ----------

func TestAggregateSSEResponse(t *testing.T) {
	t.Run("normal SSE aggregation", func(t *testing.T) {
		sseBody := strings.Join([]string{
			`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			``,
			`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			``,
			`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			``,
			`data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
			``,
			`data: [DONE]`,
		}, "\n")

		result, ct, err := aggregateSSEResponse([]byte(sseBody))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ct != "application/json" {
			t.Fatalf("expected Content-Type application/json, got %q", ct)
		}

		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("failed to parse result JSON: %v", err)
		}

		if parsed["id"] != "chatcmpl-abc" {
			t.Errorf("expected id chatcmpl-abc, got %v", parsed["id"])
		}
		if parsed["object"] != "chat.completion" {
			t.Errorf("expected object chat.completion, got %v", parsed["object"])
		}
		if parsed["model"] != "gpt-4" {
			t.Errorf("expected model gpt-4, got %v", parsed["model"])
		}

		choices, ok := parsed["choices"].([]any)
		if !ok || len(choices) == 0 {
			t.Fatalf("expected choices array, got %v", parsed["choices"])
		}
		choice := choices[0].(map[string]any)
		msg := choice["message"].(map[string]any)
		if msg["content"] != "Hello world" {
			t.Errorf("expected content 'Hello world', got %q", msg["content"])
		}
		if msg["role"] != "assistant" {
			t.Errorf("expected role assistant, got %v", msg["role"])
		}
		if choice["finish_reason"] != "stop" {
			t.Errorf("expected finish_reason stop, got %v", choice["finish_reason"])
		}

		usageMap, ok := parsed["usage"].(map[string]any)
		if !ok {
			t.Fatalf("expected usage map, got %v", parsed["usage"])
		}
		if usageMap["prompt_tokens"] != float64(10) {
			t.Errorf("expected prompt_tokens 10, got %v", usageMap["prompt_tokens"])
		}
		if usageMap["completion_tokens"] != float64(2) {
			t.Errorf("expected completion_tokens 2, got %v", usageMap["completion_tokens"])
		}
		if usageMap["total_tokens"] != float64(12) {
			t.Errorf("expected total_tokens 12, got %v", usageMap["total_tokens"])
		}
	})

	t.Run("SSE without usage", func(t *testing.T) {
		sseBody := strings.Join([]string{
			`data: {"id":"chatcmpl-xyz","object":"chat.completion.chunk","created":9876543210,"model":"gpt-3.5","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`,
			``,
			`data: {"id":"chatcmpl-xyz","object":"chat.completion.chunk","created":9876543210,"model":"gpt-3.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: [DONE]`,
		}, "\n")

		result, ct, err := aggregateSSEResponse([]byte(sseBody))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ct != "application/json" {
			t.Fatalf("expected Content-Type application/json, got %q", ct)
		}

		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("failed to parse result JSON: %v", err)
		}

		// usage should be absent
		if _, exists := parsed["usage"]; exists {
			t.Errorf("did not expect usage field, got %v", parsed["usage"])
		}

		choices := parsed["choices"].([]any)
		choice := choices[0].(map[string]any)
		msg := choice["message"].(map[string]any)
		if msg["content"] != "Hi" {
			t.Errorf("expected content 'Hi', got %q", msg["content"])
		}
	})

	t.Run("empty body", func(t *testing.T) {
		_, _, err := aggregateSSEResponse(nil)
		if err == nil {
			t.Fatal("expected error for nil body")
		}
		if !strings.Contains(err.Error(), "empty body") {
			t.Errorf("expected 'empty body' in error, got %v", err)
		}
	})

	t.Run("non-SSE data", func(t *testing.T) {
		_, _, err := aggregateSSEResponse([]byte(`{"id":"chatcmpl-abc","object":"chat.completion"}`))
		if err == nil {
			t.Fatal("expected error for non-SSE data")
		}
		if !strings.Contains(err.Error(), "no valid SSE data chunks") {
			t.Errorf("expected 'no valid SSE data chunks' in error, got %v", err)
		}
	})

	t.Run("only DONE marker", func(t *testing.T) {
		_, _, err := aggregateSSEResponse([]byte("data: [DONE]\n"))
		if err == nil {
			t.Fatal("expected error for only DONE marker")
		}
	})

	t.Run("SSE with created as integer", func(t *testing.T) {
		sseBody := `data: {"id":"c1","created":1700000000,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n"
		result, _, err := aggregateSSEResponse([]byte(sseBody))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("parse error: %v", err)
		}
		// created should be preserved as number
		if parsed["created"] != float64(1700000000) {
			t.Errorf("expected created 1700000000, got %v", parsed["created"])
		}
	})

	t.Run("multiple content deltas", func(t *testing.T) {
		sseBody := strings.Join([]string{
			`data: {"id":"c2","created":100,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"A"},"finish_reason":null}]}`,
			`data: {"id":"c2","created":100,"model":"m","choices":[{"index":0,"delta":{"content":"B"},"finish_reason":null}]}`,
			`data: {"id":"c2","created":100,"model":"m","choices":[{"index":0,"delta":{"content":"C"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n")

		result, _, err := aggregateSSEResponse([]byte(sseBody))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		json.Unmarshal(result, &parsed)
		choices := parsed["choices"].([]any)
		msg := choices[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != "ABC" {
			t.Errorf("expected 'ABC', got %q", msg["content"])
		}
	})
}

// ---------- buildTargetStates SSEAutoAggregate default ----------

func TestBuildTargetStates_SSEAutoAggregateDefault(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }

	tests := []struct {
		name     string
		input    *bool
		expected bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", boolPtr(true), true},
		{"explicit false", boolPtr(false), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgTargets := []config.Target{
				{
					Name:             "test",
					EndpointType:     "openai",
					Endpoint:         "https://api.openai.com",
					APIKey:           "sk-test",
					SSEAutoAggregate: tt.input,
				},
			}

			byName, _, err := buildTargetStates(cfgTargets)
			if err != nil {
				t.Fatalf("buildTargetStates error: %v", err)
			}

			state, ok := byName["test"]
			if !ok {
				t.Fatal("target 'test' not found")
			}
			if state.Target().SSEAutoAggregate != tt.expected {
				t.Errorf("SSEAutoAggregate = %v, want %v", state.Target().SSEAutoAggregate, tt.expected)
			}
		})
	}
}
