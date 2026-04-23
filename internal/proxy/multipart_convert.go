// multipart_convert.go — multipart/form-data → JSON 自动转换。
//
// 当上游 target 只接受 application/json（如部分 API 网关），但客户端发送了
// multipart/form-data（如 OpenAI /images/edits），代理层自动将 multipart body
// 转换为等效的 JSON body，使请求能透明通过。
//
// 文件字段转为 base64 内联：[{type: "base64", media_type: "<mime>", data: "<b64>"}]
// 文本字段保持原样，数值字段（如 "n"）自动转为数字类型。
package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

// imageEndpointSuffixes 列出需要 multipart→JSON 转换的 API 端点后缀。
var imageEndpointSuffixes = []string{
	"/images/edits",
	"/images/variations",
}

// numericFields 列出应当转为数字类型的 multipart 文本字段。
var numericFields = map[string]struct{}{
	"n": {},
}

// needsMultipartConvert 判断请求是否需要 multipart→JSON 转换。
// 条件：Content-Type 是 multipart/form-data 且路径匹配图片编辑端点。
func needsMultipartConvert(r *http.Request) bool {
	if r == nil {
		return false
	}
	ct := r.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(ct), "multipart/form-data") {
		return false
	}
	pathLower := strings.ToLower(r.URL.Path)
	for _, suffix := range imageEndpointSuffixes {
		if strings.HasSuffix(pathLower, suffix) {
			return true
		}
	}
	return false
}

// convertMultipartToJSON 将 multipart/form-data body 转换为等效的 JSON body。
//
// 返回值：
//   - jsonBody: 转换后的 JSON bytes
//   - contentType: "application/json"
//   - err: 解析或转换错误
//
// 转换规则：
//   - 有 filename 的 part（文件字段）→ data URI 字符串 "data:<mime>;base64,<b64>"
//   - 同名文件字段多次出现 → 聚合为 data URI 字符串数组
//   - 单文件字段 → 直接输出字符串（而非数组）
//   - 无 filename 的 part（文本字段）→ 字符串值（numericFields 转为数字）
//
// 该格式与 OpenAI /images/edits API 的 image 参数兼容（String | Array[String]）。
func convertMultipartToJSON(r *http.Request, body []byte) ([]byte, string, error) {
	if r == nil || len(body) == 0 {
		return nil, "", errors.New("empty request or body")
	}

	ct := r.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, "", errors.New("invalid Content-Type: " + err.Error())
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, "", errors.New("missing multipart boundary")
	}

	// 解析结果：文本字段和文件字段分开收集
	textFields := make(map[string]string)
	fileFields := make(map[string][]string) // data URI 字符串

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			return nil, "", errors.New("multipart parse error: " + partErr.Error())
		}

		fieldName := part.FormName()
		fileName := part.FileName()

		if fileName != "" {
			// 文件字段 → 读取内容并转为 data URI
			data, readErr := io.ReadAll(part)
			_ = part.Close()
			if readErr != nil {
				return nil, "", errors.New("read file part error: " + readErr.Error())
			}

			// 确定 MIME 类型
			partCT := part.Header.Get("Content-Type")
			if partCT == "" {
				partCT = detectMimeType(fileName)
			}

			dataURI := "data:" + partCT + ";base64," + base64.StdEncoding.EncodeToString(data)
			fileFields[fieldName] = append(fileFields[fieldName], dataURI)
		} else {
			// 文本字段 → 读取值
			data, readErr := io.ReadAll(io.LimitReader(part, 64*1024)) // 文本字段限 64KB
			_ = part.Close()
			if readErr != nil {
				return nil, "", errors.New("read text part error: " + readErr.Error())
			}
			textFields[fieldName] = strings.TrimSpace(string(data))
		}
	}

	// 组装 JSON payload
	payload := make(map[string]any, len(textFields)+len(fileFields))

	for k, v := range textFields {
		if _, isNumeric := numericFields[k]; isNumeric {
			if n, parseErr := strconv.Atoi(v); parseErr == nil {
				payload[k] = n
				continue
			}
		}
		payload[k] = v
	}

	for k, entries := range fileFields {
		if len(entries) == 1 {
			// 单文件 → 字符串（兼容 OpenAI image: String 格式）
			payload[k] = entries[0]
		} else {
			// 多文件 → 字符串数组（兼容 OpenAI image: Array[String] 格式）
			payload[k] = entries
		}
	}

	jsonBody, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return nil, "", errors.New("json marshal error: " + marshalErr.Error())
	}

	return jsonBody, "application/json", nil
}

// detectMimeType 根据文件名后缀推断 MIME 类型。
func detectMimeType(filename string) string {
	filename = strings.ToLower(filename)
	switch {
	case strings.HasSuffix(filename, ".png"):
		return "image/png"
	case strings.HasSuffix(filename, ".jpg"), strings.HasSuffix(filename, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(filename, ".gif"):
		return "image/gif"
	case strings.HasSuffix(filename, ".webp"):
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}
