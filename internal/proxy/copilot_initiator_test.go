package proxy

import "testing"

func TestInferInitiator(t *testing.T) {
	tests := []struct {
		name       string
		downstream string
		body       []byte
		want       string
	}{
		{
			name:       "downstream_user_wins",
			downstream: "user",
			// Body says tool_result (would be agent), but client claim wins.
			body: []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"x"}]}]}`),
			want: "user",
		},
		{
			name:       "downstream_agent_uppercase_normalized",
			downstream: "AGENT",
			body:       []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			want:       "agent",
		},
		{
			name:       "downstream_invalid_falls_through_to_body",
			downstream: "robot",
			body:       []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			want:       "user",
		},
		{
			name:       "anthropic_user_with_tool_result_is_agent",
			downstream: "",
			body:       []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"42"}]}]}`),
			want:       "agent",
		},
		{
			name:       "anthropic_user_plain_text_blocks_is_user",
			downstream: "",
			body:       []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
			want:       "user",
		},
		{
			name:       "openai_last_role_tool_is_agent",
			downstream: "",
			body:       []byte(`{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"calling tool"},{"role":"tool","content":"result"}]}`),
			want:       "agent",
		},
		{
			name:       "openai_user_string_content_is_user",
			downstream: "",
			body:       []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			want:       "user",
		},
		{
			name:       "empty_body_is_agent",
			downstream: "",
			body:       nil,
			want:       "agent",
		},
		{
			name:       "empty_byte_slice_is_agent",
			downstream: "",
			body:       []byte(""),
			want:       "agent",
		},
		{
			name:       "garbage_body_is_agent",
			downstream: "",
			body:       []byte("garbage not json"),
			want:       "agent",
		},
		{
			name:       "empty_messages_array_is_agent",
			downstream: "",
			body:       []byte(`{"messages":[]}`),
			want:       "agent",
		},
		{
			name:       "last_role_assistant_is_agent",
			downstream: "",
			body:       []byte(`{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"}]}`),
			want:       "agent",
		},
		{
			name:       "downstream_with_whitespace_user",
			downstream: "  User  ",
			body:       nil,
			want:       "user",
		},
		{
			name:       "no_messages_field_is_agent",
			downstream: "",
			body:       []byte(`{"model":"x"}`),
			want:       "agent",
		},
		{
			name:       "anthropic_mixed_blocks_with_tool_result_is_agent",
			downstream: "",
			body:       []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"see result:"},{"type":"tool_result","content":"42"}]}]}`),
			want:       "agent",
		},
		// --- OpenAI Responses API (`input` field) ---
		{
			name:       "responses_string_input",
			downstream: "",
			body:       []byte(`{"input":"hello"}`),
			want:       "user",
		},
		{
			name:       "responses_array_user_string",
			downstream: "",
			body:       []byte(`{"input":[{"role":"user","content":"hi"}]}`),
			want:       "user",
		},
		{
			name:       "responses_array_user_input_text",
			downstream: "",
			body:       []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
			want:       "user",
		},
		{
			name:       "responses_array_function_call_output_last",
			downstream: "",
			body:       []byte(`{"input":[{"role":"user","content":"q"},{"type":"function_call_output","call_id":"c1","output":"r"}]}`),
			want:       "agent",
		},
		{
			name:       "responses_array_reasoning_last",
			downstream: "",
			body:       []byte(`{"input":[{"role":"user","content":"q"},{"type":"reasoning","id":"r1"}]}`),
			want:       "agent",
		},
		{
			name:       "responses_array_assistant_message_last",
			downstream: "",
			body:       []byte(`{"input":[{"role":"user","content":"q"},{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"a"}]}]}`),
			want:       "agent",
		},
		{
			name:       "responses_array_function_call_last",
			downstream: "",
			body:       []byte(`{"input":[{"role":"user","content":"q"},{"type":"function_call","call_id":"c1","name":"x","arguments":"{}"}]}`),
			want:       "agent",
		},
		{
			name:       "responses_array_empty",
			downstream: "",
			body:       []byte(`{"input":[]}`),
			want:       "agent",
		},
		{
			name:       "responses_downstream_user_overrides_function_call_output",
			downstream: "user",
			body:       []byte(`{"input":[{"role":"user","content":"q"},{"type":"function_call_output","call_id":"c1","output":"r"}]}`),
			want:       "user",
		},
		{
			name:       "responses_array_user_with_input_image",
			downstream: "",
			body:       []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"q"},{"type":"input_image","image_url":"http://x"}]}]}`),
			want:       "user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferInitiator(tt.body, tt.downstream)
			if got != tt.want {
				t.Fatalf("inferInitiator(%q, %q) = %q, want %q",
					string(tt.body), tt.downstream, got, tt.want)
			}
		})
	}
}
