<div align="center">

<p align="center">
  <img src="docs/images/ccg.svg" alt="Claude Code & Codex CLI 智能端点轮换代理" width="720" />
</p>

[![构建状态](https://github.com/fangfsz/ccg/workflows/Build%20and%20Release/badge.svg)](https://github.com/fangfsz/ccg/actions)
[![许可证: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go 版本](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev/)
[![Wails](https://img.shields.io/badge/Wails-v2-blue)](https://wails.io/)

> ⚠️ 本项目基于 [ccNexus](https://github.com/lich0821/ccNexus) 重构。

[English](docs/README_EN.md) | [简体中文](README.md)

</div>

## 功能特性

- **多端点轮换**：自动故障转移，一个失败自动切换下一个
- **API 格式转换**：支持 Claude、OpenAI、Gemini 格式互转
- **Codex Token Pool**：支持批量导入 `access_token/refresh_token`，自动轮换、自动刷新、失效隔离与状态管理
- **Token Pool 使用统计**：单条凭证请求/错误/Token 统计，支持快捷查看
- **实时统计**：事件驱动的零延迟统计更新，支持今日/昨日/本周/本月四周期快速切换
- **端点筛选**：按类型、可用性、启用状态多选筛选，快速定位端点
- **WebDAV 同步**：多设备间同步配置和数据
- **跨平台**：Windows、macOS、Linux
- **[Docker](docs/README_DOCKER.md)**：纯后端 HTTP 服务，并提供容器化运行

## 快速开始

### 1. 下载安装

[下载最新版本](https://github.com/fangfsz/ccg/releases/latest)

- **Windows**: 解压后运行 `ccg.exe`
- **macOS**: 移动到「应用程序」，首次运行右键点击 → 打开
- **Linux**: `tar -xzf ccg-linux-amd64.tar.gz && ./ccg`

### 2. 添加端点

点击「添加端点」，填写 API 地址、密钥、选择转换器（claude/openai/gemini/openai2）。

如需使用 Codex Token Pool：

- 认证方式选择 `Codex Token Pool`
- 在 Token Pool 页面导入一批 token JSON（支持 `access_token` + `refresh_token`）
- 系统会自动进行 token 轮换、401 后刷新与状态管理（active/expiring/need_refresh/invalid 等）

### 3. 配置 CC

#### Claude Code

`~/.claude/settings.json`

```json
{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "随便写，不重要",
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:3000",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000" // 有些模型可能不支持 64k
  }
  // 其他配置
}
```

#### Codex CLI

只需要配置 `~/.codex/config.toml`：

```toml
model = "gpt-5.4"
model_provider = "ccg"
preferred_auth_method = "apikey"
approval_policy = "never"
review_model = "gpt-5.4"
model_reasoning_summary = "detailed"
model_verbosity = "high"
model_reasoning_effort = "high"
model_context_window = 1050000
model_auto_compact_token_limit = 900000
tool_output_token_limit = 100000

[model_providers.ccg]
name = "ccg"
base_url = "http://localhost:3000/v1"
wire_api = "responses"  # 或 "chat"
requires_openai_auth = true
```

`~/.codex/auth.json` 可以忽略了。

## 文档

- [详细配置](docs/configuration.md)
- [开发指南](docs/development.md)
- [常见问题](docs/FAQ.md)

## 许可证

[MIT](LICENSE)
