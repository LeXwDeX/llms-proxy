package copilot

import "net/http"

// Editor 模拟 Headers，用于 Copilot API 请求
const (
	HeaderEditorVersion = "vscode/1.96.2"
	HeaderPluginVersion = "copilot/1.254.0"
	HeaderUserAgent     = "GitHubCopilotChat/0.24.2024"
	HeaderIntegrationID = "vscode-chat"

	// GitHub Device Flow Client ID
	GitHubClientID = "Iv1.b507a08c87ecfe98"
	GitHubScope    = "read:user"

	// API 端点
	GitHubDeviceCodeURL   = "https://github.com/login/device/code"
	GitHubAccessTokenURL  = "https://github.com/login/oauth/access_token"
	CopilotTokenURL       = "https://api.github.com/copilot_internal/v2/token"
	CopilotIndividualBase = "https://api.individual.githubcopilot.com"
	CopilotChatURL        = CopilotIndividualBase + "/chat/completions"
	CopilotModelsURL      = CopilotIndividualBase + "/models"
	GitHubCopilotUserURL  = "https://api.github.com/copilot_internal/user"
)

// ApplyEditorHeaders 向请求添加编辑器模拟 headers。
func ApplyEditorHeaders(req *http.Request) {
	req.Header.Set("Editor-Version", HeaderEditorVersion)
	req.Header.Set("Editor-Plugin-Version", HeaderPluginVersion)
	req.Header.Set("User-Agent", HeaderUserAgent)
	req.Header.Set("Copilot-Integration-Id", HeaderIntegrationID)
}
