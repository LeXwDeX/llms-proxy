package proxy

import "encoding/json"

// injectBailianCacheControl 为百炼请求自动注入 cache_control 标记。
// 仅在 messages 数组中非 system 消息数量 >= 3 时注入（即第 3 轮对话开始）。
// 注入位置：
//  1. system 的最后一个 content block（如果存在 system 字段）
//  2. messages 最后一条消息的最后一个 content block
//
// 如果客户端已自行添加 cache_control，跳过注入。
// 如果 JSON 解析失败，返回原始 body。
func injectBailianCacheControl(body []byte) []byte {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}

	messages, ok := parsed["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return body
	}

	if countNonSystemMessages(messages) < 3 {
		return body
	}

	if hasCacheControl(parsed) {
		return body
	}

	// 注入 ①：system 字段（Anthropic 格式）
	if sys, exists := parsed["system"]; exists {
		parsed["system"] = injectCacheControlToContent(sys)
	}

	// 注入 ②：messages 最后一条消息
	lastMsg, ok := messages[len(messages)-1].(map[string]interface{})
	if ok {
		if content, hasContent := lastMsg["content"]; hasContent {
			lastMsg["content"] = injectCacheControlToContent(content)
		}
	}

	result, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return result
}

// hasCacheControl 检查 messages 和 system 中是否已有 cache_control 标记。
func hasCacheControl(parsed map[string]interface{}) bool {
	// 检查 system
	if sys, exists := parsed["system"]; exists {
		if blocks, ok := sys.([]interface{}); ok {
			for _, b := range blocks {
				if block, ok := b.(map[string]interface{}); ok {
					if _, has := block["cache_control"]; has {
						return true
					}
				}
			}
		}
	}

	// 检查 messages
	messages, ok := parsed["messages"].([]interface{})
	if !ok {
		return false
	}
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := msg["content"]
		if !ok {
			continue
		}
		if blocks, ok := content.([]interface{}); ok {
			for _, b := range blocks {
				if block, ok := b.(map[string]interface{}); ok {
					if _, has := block["cache_control"]; has {
						return true
					}
				}
			}
		}
	}
	return false
}

// injectCacheControlToContent 在 content 字段上注入 cache_control。
// content 可以是字符串或数组，返回修改后的值。
func injectCacheControlToContent(content interface{}) interface{} {
	cc := map[string]interface{}{"type": "ephemeral"}

	switch v := content.(type) {
	case string:
		return []interface{}{
			map[string]interface{}{
				"type":          "text",
				"text":          v,
				"cache_control": cc,
			},
		}
	case []interface{}:
		if len(v) == 0 {
			return content
		}
		lastBlock, ok := v[len(v)-1].(map[string]interface{})
		if !ok {
			return content
		}
		lastBlock["cache_control"] = cc
		return v
	default:
		return content
	}
}

// countNonSystemMessages 计算非 system 角色的消息数量。
func countNonSystemMessages(messages []interface{}) int {
	count := 0
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" {
			count++
		}
	}
	return count
}
