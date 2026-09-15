# cpa-devin-compat

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）的 C ABI 插件，修复 opencode 等基于 Vercel AI SDK 的客户端经 CPA 调用 Devin 模型（如 `devin/swe-2`）时出现的两类问题：**工具调用报错**和**首字延迟高**。

基于 CPA SDK v7.3.3 开发和实测。

## 解决的问题

| 现象 | 根因 | 插件处理 |
| --- | --- | --- |
| 走 `/v1/responses` 时，模型发出的工具调用被客户端丢弃，报 `Type validation failed` | CPA 把 Devin 响应转成 OpenAI Responses 格式时缺少必填字段：`response.created_at`、`reasoning_summary_text.delta` 的 `item_id`/`summary_index`、function_call 条目的 `status`、非流式 message 的条目 `id` 与 `annotations` | 流式分片与非流式响应里补齐这些字段 |
| 同一段对话首字时快时慢，大上下文冷启动十几秒到一分钟 | Devin 的 prompt cache 以会话 ID（cascade_id）为键。客户端不带会话头时 CPA 用前缀匹配（LCP）推断会话，历史一被改写就换新会话 ID，缓存全部失效 | 请求没有任何显式会话标识时，按「系统提示词 + 首条用户消息」补一个稳定的 `X-Session-Id`；客户端自带会话标识时不做任何改动 |
| Responses 请求带 `namespace` 工具（常见于 MCP 工具）时上游返回 `invalid_argument` 或 `MCP configuration issue` | CPA 把 namespace 转成工具分组，Devin 执行器会发出无名或不兼容工具；Codex Desktop 还会把声明放在 `input[].additional_tools` 而不是顶层 `tools` | 同时扫描顶层 `tools` 和 Codex `additional_tools`，展开 namespace 中的 function/custom 子工具（保留原名，与已有工具重名的跳过） |
| Codex Desktop 通过 `/v1/responses` 调用时，Devin 立即返回 `invalid_argument: an internal error occurred` | 个别真实工具（已定位到 `automation_update`）的 `parameters` 包含复杂 `$defs`/`$ref` 引用图；Devin 对这种引用图不兼容，即使工具数量和请求体大小都未超限也会拒绝 | 在发往上游前内联本地 JSON Schema 引用并移除定义表；递归引用边降级为 `{}`，避免无限展开 |
| chat/completions 流里第一个工具调用的 `index` 是 1 | CPA 用内部步骤下标作为 `tool_calls.index` | 映射为从 0 开始连续编号 |

插件只处理命中 `models` 的请求，默认只对 `devin/*` 生效，其他模型原样放行。

## 配置

在 CPA 的 `config.yaml` 里加入插件条目即可，未写的字段使用默认值：

```yaml
plugins:
  enabled: true
  configs:
    cpa-devin-compat:
      enabled: true
      # 以下均为默认值
      models: ["devin/*"]              # path.Match 通配符，"*" 不跨越 "/"
      session-pin: true                # 无会话标识时补稳定会话头
      session-header: X-Session-Id     # 补写的会话头名称
      flatten-namespace-tools: true    # 展开顶层 tools 和 Codex additional_tools 的 namespace 工具
      sanitize-tool-schemas: true      # 内联本地 $defs/$ref，兼容 Devin 工具 schema
      fix-responses: true              # 补齐 Responses 缺失字段
      normalize-chat-tool-index: true  # chat 流 tool_calls.index 从 0 编号
      log: true                        # 记录补写与修补次数
```

## 构建与安装

需要 Go 1.26 与 CGO（gcc）：

```bash
CGO_ENABLED=1 go build -trimpath -buildvcs=false -buildmode=c-shared \
  -ldflags="-s -w" -o cpa-devin-compat-v0.1.2.so .
```

把生成的 `.so` 放到 CPA 的插件目录 `plugins/linux/amd64/`（相对 CPA 工作目录），再在配置里加上插件条目。首次加入配置会热加载；替换同名插件的新版本需要重启 CPA。

Docker 部署时注意把插件目录挂载到宿主机，例如 `./plugins:/CLIProxyAPI/plugins`，否则更新镜像、重建容器后插件会丢失。

加载成功后日志里会出现：

```
pluginhost: plugin registered plugin_id=cpa-devin-compat ... version=0.1.2
```

运行时日志示例：

```
[cpa-devin-compat] session-pin source=openai-response model=devin/swe-2 header=X-Session-Id id=dvc_ctx:v1:52c8e
[cpa-devin-compat] responses-fix stream request=... fixes=12
[cpa-devin-compat] namespace-flatten model=devin/swe-2 namespaces=1 tools=3
[cpa-devin-compat] schema-inline model=devin/swe-2 schemas=1 refs=53
```

## 测试

```bash
go test ./...
```

曾用隔离的 CPA v7.3.3 实例（真实 Devin 凭据）加 AI SDK 7.0.101、`@ai-sdk/openai` 4.0.66、`@ai-sdk/openai-compatible` 3.0.48 做端到端验证：Responses 流式（`store` 为 true/false）、非流式和 chat 流式都能完成多步并行工具调用；namespace 工具返回 200；历史被改写后缓存命中从 0 提升到 8192 token。

## 已知限制

- 全新对话首轮的大上下文冷计算发生在 Devin 上游，插件无法缩短，只能保证后续轮次命中缓存。
- Responses 的 WebSocket 通道未做端到端测试（代码上与 HTTP 流共用同一条分片拦截路径）。
- namespace 内的 Codex `custom` 工具（如 `apply_patch`）会被保留并交给 CPA v7.3.3 转换；OpenAI 原生 `web_search` 等服务端工具仍取决于 CPA/Devin 上游支持。

## License

MIT
