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
	"/images/edits",
	"/images/variations",
	"/embeddings",
}

// 网宿图像通道（独立终态 URL），各自只接受一条客户端路径，用以路由消歧。
var (
	wangsuOpenAIImageSupportedPaths     = []string{"/images/generations"}
	wangsuOpenAIImageEditSupportedPaths = []string{"/images/edits"}
)

// PathSupportedByEndpointType 检查指定 endpoint_type 是否支持给定的请求路径。
// wangsu_openai / wangsu_openai_image / wangsu_openai_image_edit 有路径限制；其余类型全放行。
func PathSupportedByEndpointType(epType, path string) bool {
	pathLower := strings.ToLower(path)
	switch epType {
	case config.EndpointTypeWangsuOpenAI:
		return pathHasAnySuffix(pathLower, wangsuOpenAISupportedPaths)
	case config.EndpointTypeWangsuOpenAIImage:
		return pathHasAnySuffix(pathLower, wangsuOpenAIImageSupportedPaths)
	case config.EndpointTypeWangsuOpenAIImageEdit:
		return pathHasAnySuffix(pathLower, wangsuOpenAIImageEditSupportedPaths)
	}
	return true
}

func pathHasAnySuffix(pathLower string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(pathLower, suf) {
			return true
		}
	}
	return false
}
