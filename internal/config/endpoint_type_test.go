package config

import "testing"

// TestEndpointTypeMetaSingleSourceOfTruth 验证 ValidEndpointTypes 与 endpointTypes
// 元数据保持一致，防止后续有人在两处重复维护出现漂移。
func TestEndpointTypeMetaSingleSourceOfTruth(t *testing.T) {
	metas := AllEndpointTypeMetas()
	if len(metas) != len(ValidEndpointTypes) {
		t.Fatalf("ValidEndpointTypes(%d) 与 AllEndpointTypeMetas(%d) 长度不一致，存在漂移", len(ValidEndpointTypes), len(metas))
	}
	for i, m := range metas {
		if m.Code != ValidEndpointTypes[i] {
			t.Errorf("索引 %d 不一致：meta.Code=%q ValidEndpointTypes=%q", i, m.Code, ValidEndpointTypes[i])
		}
		if m.DisplayName == "" {
			t.Errorf("%s 缺少 DisplayName（UI 必需）", m.Code)
		}
		if m.ShortLabel == "" {
			t.Errorf("%s 缺少 ShortLabel（UI 必需）", m.Code)
		}
		if m.BadgeBackground == "" || m.BadgeForeground == "" {
			t.Errorf("%s 缺少徽章配色", m.Code)
		}
	}
}

func TestEndpointTypeMetaOf(t *testing.T) {
	// 标准 code 命中
	m, ok := EndpointTypeMetaOf("wangsu_openai_image")
	if !ok {
		t.Fatal("未找到 wangsu_openai_image 元数据")
	}
	if m.DisplayName != "网宿 OpenAI 文生图" {
		t.Errorf("DisplayName 不符合预期：%s", m.DisplayName)
	}
	// 大小写 + 空白容错
	if _, ok := EndpointTypeMetaOf("  WANGSU_OPENAI_IMAGE_EDIT  "); !ok {
		t.Error("大小写/空白未正规化")
	}
	// 未知类型返回 false
	if _, ok := EndpointTypeMetaOf("unknown_type"); ok {
		t.Error("未知类型应返回 false")
	}
}

// TestRequiresResourcePathPrefixOnlyAzure 防止有人误把 RequiresResourcePathPrefix
// 标记到非 azure_openai 上（业务上目前仅 azure_openai 需要）。
func TestRequiresResourcePathPrefixOnlyAzure(t *testing.T) {
	for _, m := range AllEndpointTypeMetas() {
		if m.RequiresResourcePathPrefix && m.Code != EndpointTypeAzureOpenAI {
			t.Errorf("%s 不应需要 RequiresResourcePathPrefix（目前仅 azure_openai 需要）", m.Code)
		}
	}
}
