# newapi-box

一个轻量的 **LLM API 协议转换网关**：对外同时暴露 4 种客户端协议，请求经内核转换后转发给**唯一一个可随时切换的上游**，并内置 Web 控制台，改配置无需重启、不丢在途流。

> **内核来源说明**：本项目的协议转换内核 [`relaykit/`](relaykit/)（DTO 定义、转换注册表、各协议转换器）源自开源项目 [new-api](https://github.com/QuantumNous/new-api) 的 relay 代码，经抽取、裁剪和重构后形成独立转换内核；`internal/` 下的 HTTP 服务、控制台、配置存储与 `desktop/` 桌面壳为本项目原创实现。

## 它解决什么问题

客户端只会说一种协议，上游只认另一种协议——比如 Claude Code 只能发 Claude Messages，而你的上游是 OpenAI 兼容接口。newapi-box 站在中间做翻译：

```
client: OpenAI Chat | OpenAI Responses | Claude Messages | Gemini
                          |
                   relaykit 内核（源自 new-api）
                          |
upstream:        一个已配置的协议，可在运行时切换
```

数据通路是单向无状态的：

```
客户端协议 --decode--> DTO --relaykit 转换--> DTO --encode--> 上游
```

## 特性

- **4 种客户端协议并存**，按请求路径自动识别：
  | 客户端协议 | 路径 |
  |---|---|
  | OpenAI Chat Completions | `POST /v1/chat/completions` |
  | OpenAI Responses | `POST /v1/responses` |
  | Claude Messages | `POST /v1/messages` |
  | Gemini | `POST /v1beta/models/{model}:generateContent` / `:streamGenerateContent` |
- **上游协议可运行时切换**：控制台保存后立即生效（`atomic.Pointer` 原子发布快照），不中断在途流。
- **内置 Web 控制台**（`/`）：编辑上游地址/密钥/模型映射/自定义 header、一键探测上游连通性、查看近期请求事件。与转发路径共享同一份 `config.Store`，只有一个事实来源。
- **SSE 流式转换**：请求/响应/流全程在四种协议间互转。
- **模型映射**：客户端请求的模型名可映射为上游实际模型名（`model_map`）。
- **可选 Admin Key**：控制台 API 支持常量时间比较鉴权；留空则为本地免鉴权模式（UI 会提示）。
- **双形态交付**：
  - **命令行版**（`main.go`）：标准 HTTP 服务，`-port` / `-listen` 可临时覆盖配置文件里的监听地址。
  - **桌面版**（`desktop/`，Windows）：服务与 WebView2 控制台窗口跑在**同一个进程**，带托盘图标、单实例互斥，退出不留后台进程，堆上限 128MB。

## 快速开始

```bash
# 构建
go build -o newapi-box.exe .

# 启动（首次运行自动生成 config.json，上游为空，去控制台补全即可）
./newapi-box.exe
# 日志会打印控制台地址，默认 http://localhost:18888/
```

指定端口或完整监听地址（仅启动时生效，不写回配置文件）：

```bash
./newapi-box.exe -port 9000
./newapi-box.exe -listen 127.0.0.1:9000
```

### 配置示例（config.json）

```json
{
  "listen": ":18888",
  "admin_key": "",
  "claude_default_max_tokens": 8192,
  "upstream": {
    "format": "openai",
    "base_url": "https://api.openai.com",
    "api_key": "sk-...",
    "timeout": "5m",
    "model_map": {
      "claude-sonnet-4-5": "gpt-4o"
    }
  }
}
```

`upstream.format` 支持 `openai` / `openai_responses` / `claude` / `gemini`。完整字段见 [config.example.json](config.example.json)。

### 桌面版（Windows）

```bash
cd desktop
./build.sh          # 产物 dist/newapi-box.exe
MINIFY=1 ./build.sh # 额外生成约 2.7MB 的精简版（个别杀软可能误报）
```

## 控制台 API

管理端点挂在 `/admin/api/` 下，鉴权方式为 `Authorization: Bearer <admin_key>` 或 `x-admin-key` 头：

| 端点 | 说明 |
|---|---|
| `GET /admin/api/status` | 运行状态（监听地址、启动时间等） |
| `GET/PUT /admin/api/config` | 读取/保存上游配置（保存即生效） |
| `POST /admin/api/test` | 探测上游连通性 |
| `GET /admin/api/events` | 近期请求事件日志 |

## 项目结构

```
main.go                 命令行版入口（HTTP 服务 + 优雅退出）
internal/
  admin/                内嵌控制台 UI 与 JSON API（含鉴权）
  config/               配置加载与线程安全存储（Store）
  proxy/                转发主体：HTTP 组帧、headers、SSE、错误形态
  protocol/             入站协议识别、请求解码、流式封装（SSE）
desktop/                Windows 桌面壳（WebView2 窗口 + 托盘 + 单实例）
  build.sh              桌面版构建脚本（图标/版本信息/编译）
relaykit/               ★ 协议转换内核（源自 new-api，抽取重构）
  dto/                  各协议请求/响应 DTO（OpenAI/Claude/Gemini/音频/图像等）
  types/                RelayFormat、转换元数据、错误类型
  relayconvert/         转换器注册表与各协议转换器（含黄金测试）
  reasonmap/            推理内容映射
  convmeta/  kitutil/   转换元信息与工具包
vendor/                 Go 依赖（go mod vendor）
```

## 测试

```bash
go test ./...
```

内核带黄金测试（`relayconvert/testdata`）与边界测试，`internal/` 各包均有单测。

## 致谢

- [new-api](https://github.com/QuantumNous/new-api) —— `relaykit/` 转换内核的代码来源
- 依赖：[tidwall/gjson](https://github.com/tidwall/gjson)、[samber/lo](https://github.com/samber/lo)、[jchv/go-webview2](https://github.com/jchv/go-webview2) 等，详见 [go.mod](go.mod)
