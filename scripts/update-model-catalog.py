#!/usr/bin/env python3
"""
从 models.dev/api.json 生成项目内部模型目录。

数据源的价格单位是 $/百万 tokens。本脚本转换为 $/千 tokens 存储。

用法:
  curl -sS https://models.dev/api.json -o /tmp/models_dev_raw.json
  python3 scripts/update-model-catalog.py [input.json] [output.json]

默认:
  input  = /tmp/models_dev_raw.json
  output = internal/catalog/data/models.json
"""
import json
import sys
from typing import Any, Optional


# models.dev provider id -> 本项目 endpoint_type 的映射
PROVIDER_MAP = {
    "openai": "openai",
    "azure": "azure_openai",
    "anthropic": "claude",
}


def convert_cost(cost_obj: dict) -> Optional[dict]:
    """将 models.dev 的 cost ($/M tokens) 转为项目的 $/K tokens。"""
    if not cost_obj:
        return None

    result: dict[str, float] = {}

    input_price = cost_obj.get("input")
    output_price = cost_obj.get("output")
    cached_price = cost_obj.get("cache_read")

    if isinstance(input_price, (int, float)) and input_price > 0:
        result["input_per_1k_tokens"] = round(input_price / 1000, 10)
    if isinstance(output_price, (int, float)) and output_price > 0:
        result["output_per_1k_tokens"] = round(output_price / 1000, 10)
    if isinstance(cached_price, (int, float)) and cached_price > 0:
        result["cached_input_per_1k_tokens"] = round(cached_price / 1000, 10)

    return result if result else None


def extract_capabilities(m: dict) -> list[str]:
    """从模型元数据中提取 capabilities 标签。"""
    caps = []
    modalities = m.get("modalities", {})
    input_mods = modalities.get("input", [])
    output_mods = modalities.get("output", [])

    if "image" in input_mods:
        caps.append("vision")
    if "image" in output_mods:
        caps.append("image_generation")
    if "audio" in input_mods:
        caps.append("audio_input")
    if "audio" in output_mods:
        caps.append("audio_output")
    if m.get("tool_call"):
        caps.append("function_calling")
    if m.get("structured_output"):
        caps.append("structured_output")
    if m.get("reasoning"):
        caps.append("reasoning")
    return caps


def transform_model(provider_id: str, model_id: str, m: dict) -> Optional[dict]:
    """将外部模型数据转换为项目内部格式。"""
    endpoint_type = PROVIDER_MAP.get(provider_id)
    if not endpoint_type:
        return None

    entry: dict[str, Any] = {
        "endpoint_type": endpoint_type,
        "model": model_id.lower().strip(),
    }

    # display_name
    name = m.get("name", "")
    if name and name.lower() != model_id.lower():
        entry["display_name"] = name

    # 费用
    cost = convert_cost(m.get("cost", {}))
    if cost:
        entry["default_cost"] = cost

    # capabilities
    caps = extract_capabilities(m)
    if caps:
        entry["capabilities"] = caps

    return entry


def _supplementary_models() -> list[dict]:
    """models.dev 中缺失但项目需要的模型条目（估算价格，仅供参考）。"""
    extras: list[dict] = []

    # --- OpenAI: 图像、音频类模型 ---
    image_models = [
        ("dall-e-3", "DALL·E 3", {"input_per_1k_tokens": 0.04, "output_per_1k_tokens": 0.08}),
        ("dall-e-2", "DALL·E 2", {"input_per_1k_tokens": 0.02, "output_per_1k_tokens": 0.02}),
        ("gpt-image-1", "GPT Image 1", {"input_per_1k_tokens": 0.005, "output_per_1k_tokens": 0.04}),
        ("gpt-image-1.5", "GPT Image 1.5", {"input_per_1k_tokens": 0.005, "output_per_1k_tokens": 0.04}),
    ]
    for model_id, name, cost in image_models:
        extras.append({
            "endpoint_type": "openai",
            "model": model_id,
            "display_name": name,
            "default_cost": cost,
            "capabilities": ["image_generation"],
        })

    audio_models = [
        ("tts-1", "TTS 1", {"input_per_1k_tokens": 0.015}),
        ("tts-1-hd", "TTS 1 HD", {"input_per_1k_tokens": 0.03}),
        ("whisper-1", "Whisper 1", {"input_per_1k_tokens": 0.006}),
    ]
    for model_id, name, cost in audio_models:
        extras.append({
            "endpoint_type": "openai",
            "model": model_id,
            "display_name": name,
            "default_cost": cost,
            "capabilities": ["audio_input"] if "whisper" in model_id else ["audio_output"],
        })

    # --- OpenAI: gpt-5.2-chat (azure 有 但 openai provider 缺) ---
    extras.append({
        "endpoint_type": "openai",
        "model": "gpt-5.2-chat",
        "display_name": "GPT-5.2 Chat",
        "default_cost": {
            "input_per_1k_tokens": 0.002,
            "output_per_1k_tokens": 0.008,
            "cached_input_per_1k_tokens": 0.0002,
        },
        "capabilities": ["vision", "function_calling", "structured_output"],
    })

    # --- Azure OpenAI: 同样补充图像和音频模型 ---
    for model_id, name, cost in image_models:
        extras.append({
            "endpoint_type": "azure_openai",
            "model": model_id,
            "display_name": name,
            "default_cost": cost,
            "capabilities": ["image_generation"],
        })
    for model_id, name, cost in audio_models:
        extras.append({
            "endpoint_type": "azure_openai",
            "model": model_id,
            "display_name": name,
            "default_cost": cost,
            "capabilities": ["audio_input"] if "whisper" in model_id else ["audio_output"],
        })

    # --- Claude: 仅保留 dot-notation 变体作为独立条目 ---
    # 注意: claude-sonnet-4, claude-opus-4, claude-4-sonnet, claude-4-opus, claude-sonnet-4.5
    #       以及 claude-3.5-* 变体均通过 _add_aliases() 添加为别名，不再创建独立条目。

    return extras


def _add_aliases(entries: list[dict]) -> None:
    """为已有条目添加别名映射。"""
    alias_map: dict[tuple[str, str], list[str]] = {
        # Claude: 点号 -> 连字符 映射
        ("claude", "claude-3-5-sonnet-20241022"): ["claude-3.5-sonnet-20241022"],
        ("claude", "claude-3-5-haiku-20241022"): ["claude-3.5-haiku-20241022"],
        ("claude", "claude-3-5-haiku-latest"): ["claude-3.5-haiku-latest"],
        ("claude", "claude-3-5-sonnet-20240620"): ["claude-3.5-sonnet-20240620"],
        ("claude", "claude-3-7-sonnet-20250219"): ["claude-3.7-sonnet-20250219"],
        ("claude", "claude-3-7-sonnet-latest"): ["claude-3.7-sonnet-latest"],
        # Claude: 短名 -> 带日期名
        ("claude", "claude-sonnet-4-20250514"): ["claude-sonnet-4", "claude-4-sonnet"],
        ("claude", "claude-opus-4-20250514"): ["claude-opus-4", "claude-4-opus"],
        ("claude", "claude-sonnet-4-5-20250929"): ["claude-sonnet-4.5"],
    }

    for entry in entries:
        key = (entry["endpoint_type"], entry["model"])
        if key in alias_map:
            existing = entry.get("aliases", [])
            for a in alias_map[key]:
                if a not in existing:
                    existing.append(a)
            if existing:
                entry["aliases"] = existing


def main():
    input_path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/models_dev_raw.json"
    output_path = (
        sys.argv[2] if len(sys.argv) > 2 else "internal/catalog/data/models.json"
    )

    with open(input_path, "r") as f:
        raw = json.load(f)

    entries = []

    for provider_id, endpoint_type in PROVIDER_MAP.items():
        provider = raw.get(provider_id)
        if not provider:
            print(f"警告: 数据中未找到 provider '{provider_id}'，跳过")
            continue

        models = provider.get("models", {})
        if not isinstance(models, dict):
            print(f"警告: provider '{provider_id}' 的 models 不是对象，跳过")
            continue

        count = 0
        for model_id, model_data in models.items():
            entry = transform_model(provider_id, model_id, model_data)
            if entry:
                entries.append(entry)
                count += 1

        print(f"  {provider_id} ({endpoint_type}): {count} 个模型")

    # --- 补充 models.dev 中缺失的常见模型 ---
    existing_keys = {(e["endpoint_type"], e["model"]) for e in entries}

    supplementary = _supplementary_models()
    added = 0
    for s in supplementary:
        key = (s["endpoint_type"], s["model"])
        if key not in existing_keys:
            entries.append(s)
            existing_keys.add(key)
            added += 1

    if added:
        print(f"  补充缺失模型: {added} 个")

    # --- 为特定模型添加别名 ---
    _add_aliases(entries)

    # 按 endpoint_type + model 排序
    entries.sort(key=lambda e: (e["endpoint_type"], e["model"]))

    with open(output_path, "w") as f:
        json.dump(entries, f, indent=2, ensure_ascii=False)

    print(f"\n共生成 {len(entries)} 个模型条目 -> {output_path}")


if __name__ == "__main__":
    main()
