package proxy

import (
	"strings"

	"github.com/ycgame/llms-proxy/internal/config"
)

// wangsuOpenAISupportedPaths 列出 wangsu_openai 支持的请求路径后缀。
// 网宿 OpenAI 兼容通道仅支持部分 OpenAI 端点，不支持 /responses 等。
var wangsuOpenAISupportedPaths = []string{
	"/chat/completions",
	"/images/generations",
	"/embeddings",
}

// PathSupportedByEndpointType 检查指定 endpoint_type 是否支持给定的请求路径。
// 仅 wangsu_openai 有路径限制；其余类型全放行。
func PathSupportedByEndpointType(epType, path string) bool {
	if epType != config.EndpointTypeWangsuOpenAI {
		return true
	}
	pathLower := strings.ToLower(path)
	for _, supported := range wangsuOpenAISupportedPaths {
		if strings.HasSuffix(pathLower, supported) {
			return true
		}
	}
	return false
}
