// endpoint_hint.go — 子路由（如 /deepseek/*）通过 context 注入「endpoint_type 路由约束」，
// 让 selectTarget 仅在指定 endpoint_type 的 target 池中做选择，避免误路由到其他类型。
package proxy

import (
	"context"
	"strings"
)

type endpointTypeHintKey struct{}

// WithEndpointTypeHint 在 context 中注入 endpoint_type 约束。
// 同名 hint 多次注入时取最后一次。空字符串视为未约束。
func WithEndpointTypeHint(ctx context.Context, epType string) context.Context {
	epType = strings.ToLower(strings.TrimSpace(epType))
	if epType == "" {
		return ctx
	}
	return context.WithValue(ctx, endpointTypeHintKey{}, epType)
}

// EndpointTypeHintFromContext 取出 endpoint_type 约束；不存在或为空返回 ""。
func EndpointTypeHintFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, ok := ctx.Value(endpointTypeHintKey{}).(string)
	if !ok {
		return ""
	}
	return v
}
