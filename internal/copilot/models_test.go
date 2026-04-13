package copilot

import (
	"testing"
)

func TestMapModelName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantModel string
		wantFound bool
	}{
		{
			name:      "有 Copilot 前缀（规范格式）",
			input:     "Copilot gpt-4o",
			wantModel: "gpt-4o",
			wantFound: true,
		},
		{
			name:      "无前缀不映射",
			input:     "gpt-4o",
			wantModel: "gpt-4o",
			wantFound: false,
		},
		{
			name:      "大小写不敏感 - 全小写前缀",
			input:     "copilot gpt-4o",
			wantModel: "gpt-4o",
			wantFound: true,
		},
		{
			name:      "大小写不敏感 - 全大写前缀",
			input:     "COPILOT gpt-4o",
			wantModel: "gpt-4o",
			wantFound: true,
		},
		{
			name:      "大小写不敏感 - 混合大小写",
			input:     "Copilot claude-sonnet-4",
			wantModel: "claude-sonnet-4",
			wantFound: true,
		},
		{
			name:      "空字符串",
			input:     "",
			wantModel: "",
			wantFound: false,
		},
		{
			name:      "仅前缀",
			input:     "Copilot ",
			wantModel: "",
			wantFound: true,
		},
		{
			name:      "前缀不完整",
			input:     "Copilo gpt-4o",
			wantModel: "Copilo gpt-4o",
			wantFound: false,
		},
		{
			name:      "旧格式 copilot_ 不再匹配",
			input:     "copilot_gpt-4o",
			wantModel: "copilot_gpt-4o",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotModel, gotFound := MapModelName(tt.input)
			if gotModel != tt.wantModel {
				t.Errorf("MapModelName(%q) model = %q, want %q", tt.input, gotModel, tt.wantModel)
			}
			if gotFound != tt.wantFound {
				t.Errorf("MapModelName(%q) found = %v, want %v", tt.input, gotFound, tt.wantFound)
			}
		})
	}
}

func TestReverseMapModelName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "正常映射",
			input: "gpt-4o",
			want:  "Copilot gpt-4o",
		},
		{
			name:  "claude 模型",
			input: "claude-sonnet-4",
			want:  "Copilot claude-sonnet-4",
		},
		{
			name:  "空字符串",
			input: "",
			want:  "Copilot ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReverseMapModelName(tt.input)
			if got != tt.want {
				t.Errorf("ReverseMapModelName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGetMultiplier(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  float64
	}{
		// 已知免费模型
		{name: "免费模型 gpt-4o", model: "gpt-4o", want: 0},
		{name: "免费模型 gpt-4.1", model: "gpt-4.1", want: 0},
		{name: "免费模型 gpt-5-mini", model: "gpt-5-mini", want: 0},
		{name: "免费模型 raptor-mini", model: "raptor-mini", want: 0},

		// 低消耗模型
		{name: "低消耗 grok-code-fast-1", model: "grok-code-fast-1", want: 0.25},
		{name: "低消耗 claude-haiku-4.5", model: "claude-haiku-4.5", want: 0.33},

		// 标准模型
		{name: "标准 claude-sonnet-4", model: "claude-sonnet-4", want: 1},
		{name: "标准 gpt-5.1", model: "gpt-5.1", want: 1},

		// 高消耗模型
		{name: "高消耗 claude-opus-4.5", model: "claude-opus-4.5", want: 3},
		{name: "高消耗 claude-opus-4.6", model: "claude-opus-4.6", want: 3},

		// 未知模型默认 1.0
		{name: "未知模型", model: "unknown-model-xyz", want: 1.0},

		// Copilot 前缀处理
		{name: "带 Copilot 前缀的免费模型", model: "Copilot gpt-4o", want: 0},
		{name: "带 Copilot 前缀的标准模型", model: "Copilot claude-sonnet-4", want: 1},
		{name: "带 Copilot 前缀的未知模型", model: "Copilot unknown-model", want: 1.0},

		// 大小写不敏感
		{name: "大写模型名", model: "GPT-4O", want: 0},
		{name: "混合大小写", model: "Claude-Sonnet-4", want: 1},
		{name: "小写前缀", model: "copilot gpt-4o", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetMultiplier(tt.model)
			if got != tt.want {
				t.Errorf("GetMultiplier(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestListAvailableModels(t *testing.T) {
	models := ListAvailableModels()

	// 验证数量与 ModelMultipliers 一致
	if len(models) != len(ModelMultipliers) {
		t.Errorf("ListAvailableModels() 返回 %d 个模型, 期望 %d 个", len(models), len(ModelMultipliers))
	}

	// 验证按 Category 分组排序
	categoryOrder := map[string]int{
		"免费":  0,
		"低消耗": 1,
		"标准":  2,
		"高消耗": 3,
	}

	for i := 1; i < len(models); i++ {
		prevOrder := categoryOrder[models[i-1].Category]
		currOrder := categoryOrder[models[i].Category]
		if prevOrder > currOrder {
			t.Errorf("模型排序错误: models[%d].Category=%q (order %d) > models[%d].Category=%q (order %d)",
				i-1, models[i-1].Category, prevOrder, i, models[i].Category, currOrder)
		}
		// 同 Category 内按名字排序
		if prevOrder == currOrder && models[i-1].Name > models[i].Name {
			t.Errorf("同 Category 内排序错误: models[%d].Name=%q > models[%d].Name=%q",
				i-1, models[i-1].Name, i, models[i].Name)
		}
	}

	// 验证每个模型都有合法的 Category
	for _, m := range models {
		if _, ok := categoryOrder[m.Category]; !ok {
			t.Errorf("模型 %q 的 Category %q 不合法", m.Name, m.Category)
		}
	}

	// 验证免费模型数量
	freeCount := 0
	for _, m := range models {
		if m.Category == "免费" {
			freeCount++
		}
	}
	if freeCount != 4 {
		t.Errorf("免费模型数量 = %d, 期望 4", freeCount)
	}
}

func TestIsFreeModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  bool
	}{
		{name: "免费模型 gpt-4o", model: "gpt-4o", want: true},
		{name: "免费模型 gpt-5-mini", model: "gpt-5-mini", want: true},
		{name: "免费模型 raptor-mini", model: "raptor-mini", want: true},
		{name: "非免费模型 claude-sonnet-4", model: "claude-sonnet-4", want: false},
		{name: "非免费模型 claude-opus-4.5", model: "claude-opus-4.5", want: false},
		{name: "未知模型（默认乘数 1.0，非免费）", model: "unknown", want: false},
		{name: "带前缀免费模型", model: "Copilot gpt-4o", want: true},
		{name: "带前缀非免费模型", model: "Copilot claude-opus-4.5", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsFreeModel(tt.model)
			if got != tt.want {
				t.Errorf("IsFreeModel(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}
