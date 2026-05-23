package proxy

import (
	"encoding/json"
	"testing"
)

func TestInjectBailianCacheControl(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantChange bool
	}{
		{
			name:       "openai_3rounds_string_content",
			input:      `{"model":"qwen3-coder-plus","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"},{"role":"user","content":"继续"}]}`,
			wantChange: true,
		},
		{
			name:       "openai_2rounds_skip",
			input:      `{"model":"qwen3-coder-plus","messages":[{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"}]}`,
			wantChange: false,
		},
		{
			name:       "openai_1round_skip",
			input:      `{"model":"qwen3-coder-plus","messages":[{"role":"user","content":"你好"}]}`,
			wantChange: false,
		},
		{
			name:       "anthropic_3rounds_system_string",
			input:      `{"model":"qwen3-coder-plus","max_tokens":1024,"system":"你是助手","messages":[{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"},{"role":"user","content":"继续"}]}`,
			wantChange: true,
		},
		{
			name:       "client_already_has_cache_control",
			input:      `{"model":"qwen3-coder-plus","messages":[{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"},{"role":"user","content":[{"type":"text","text":"继续","cache_control":{"type":"ephemeral"}}]}]}`,
			wantChange: false,
		},
		{
			name:       "no_messages_field",
			input:      `{"model":"text-embedding-v3","input":"测试"}`,
			wantChange: false,
		},
		{
			name:       "empty_messages",
			input:      `{"model":"qwen3-coder-plus","messages":[]}`,
			wantChange: false,
		},
		{
			name:       "invalid_json",
			input:      `not json`,
			wantChange: false,
		},
		{
			name:       "openai_3rounds_array_content",
			input:      `{"model":"qwen3-coder-plus","messages":[{"role":"user","content":[{"type":"text","text":"你好"}]},{"role":"assistant","content":[{"type":"text","text":"你好！"}]},{"role":"user","content":[{"type":"text","text":"继续"}]}]}`,
			wantChange: true,
		},
		{
			name:       "system_plus_2_nonsystem_skip",
			input:      `{"model":"qwen3-coder-plus","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"}]}`,
			wantChange: false,
		},
		{
			name:       "system_plus_3_nonsystem_inject",
			input:      `{"model":"qwen3-coder-plus","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"},{"role":"user","content":"继续"}]}`,
			wantChange: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := injectBailianCacheControl([]byte(tt.input))
			changed := string(result) != tt.input
			if changed != tt.wantChange {
				t.Errorf("injectBailianCacheControl() changed=%v, want %v\ninput:  %s\noutput: %s", changed, tt.wantChange, tt.input, string(result))
			}
			if tt.wantChange {
				var parsed map[string]interface{}
				if err := json.Unmarshal(result, &parsed); err != nil {
					t.Fatalf("output is not valid JSON: %v\noutput: %s", err, string(result))
				}
				if !hasCacheControl(parsed) {
					t.Errorf("expected cache_control to be present in output: %s", string(result))
				}
			}
		})
	}
}
