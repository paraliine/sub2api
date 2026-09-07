# Sub2API Codex Bug 清单

本文记录原文中的具体实现错误、2026-09-08 的修复基线及后续审计入口。后续 agent 应先读“后续审计方法”，再判断发现的是新问题、已知未修项，还是有意保留的账号隔离行为。

## 2026-09-08 修复基线

- 实现提交：`8db97fe0f`（`fix(openai): unify Codex identity projections`）。
- `v0.2.2` 合并与部署提交：`e33fb7d4b`（`merge: sync v0.2.2 into fingerprint`）。
- 远端分支：`paraliine/fingerprint`。
- 本轮实现范围：`off` 和 `device`。`session` / `full` 的虚拟会话状态机问题有意暂缓，不属于本轮完成范围。
- 验证结果：身份专项测试、后端 `go test ./...`、前端 lint、typecheck、build 以及 262 个前端测试文件中的 1,896 个测试均通过。
- 部署结果：`v0.2.2` 已部署，运行镜像 commit 为 `e33fb7d4b`，容器健康且真实 Responses 流量正常。

本轮建立的核心约束是：先从请求中解析一份 canonical identity snapshot，再把同一结果投影到 HTTP、WebSocket 和 Compact 的各个载体。投影器只能改变载体格式，不能再次派生身份。

## 1. 身份映射破坏同一实体的引用关系

- 状态：`off` / `device` 已修复、测试通过并部署。
- 触发场景：对普通根线程进行账号身份映射，原始请求满足 `session_id == thread_id == x-client-request-id`。
- 原实现分别使用 `kind:session`、`kind:thread`、`kind:request` 派生 ID，即使原始值相同，也会得到不同结果。
- `x-client-request-id` 在该场景中是 thread identity 的引用，单独派生 request identity 会破坏这一关系。
- Bug 在于映射后未保持原有的实体引用关系。
- 当前修改：先按原始引用关系识别 identity node，再做账号隔离映射。根线程的 session/thread/request 引用保持相等；原本不同的子线程仍映射为不同 ID。parent/fork thread 引用跟随 thread 节点，turn 和窗口历史引用分别跟随 turn/window 节点，installation 保持独立节点。默认缓存键从原始 Header、平铺或嵌套 metadata 识别 session 引用。
- 本项不包含后续 `session/full` 收敛策略对身份的覆盖。

原问题示例：

```text
原始：session_id = S1，thread_id = S1，x-client-request-id = S1
改写：session_id = S'，thread_id = T'，x-client-request-id = R'
结果：原本相等的引用被拆成不同 ID
```

## 2. 窗口标识固定，压缩后无法切换到新窗口

- 状态：已知未修，仅涉及 `session` / `full`，按当前范围暂缓。
- 本地 Codex 的 `current_window()` 实际返回 `<thread_id>:<window_number>`，独立 UUID 为 `context_window_id`。拼接格式本身不列为 bug。
- 正常压缩后，线程 ID 保持不变，当前窗口 ID 发生变化，`window_number` 递增。
- `session` / `full` 固定生成 `<thread_id>:0`，同一线程压缩前后得到相同的窗口标识。
- Bug 在于不同压缩窗口被映射为同一个 window，窗口切换信息丢失。
- 硬编码的是拼接结果中的 `:0`；`window_number` 仍可能保留客户端的旧值。

## 3. 部分字段改写导致窗口 metadata 状态不一致

- 状态：已知未修，仅涉及 `session` / `full`，按当前范围暂缓。
- `session` / `full` 会改写 thread、turn、window 等字段，但合法 metadata 中未被指定改写的字段仍可能保留。
- 因而可能出现新的 `thread_id` / `window_id` 与旧的 `window_number`、`context_window_id` 同时存在。
- Bug 在于改写没有成套维护窗口身份、计数及上下文关系，形成新旧状态混合。

示例：

```text
thread_id         = NEW_THREAD
window_id         = NEW_THREAD:0
window_number     = 3
context_window_id = OLD_CONTEXT
```

## 4. `session` 模式改写线程归属后缺少对应 lineage

- 状态：已知未修，按当前范围暂缓。
- 多个无关客户端 session 被收敛到同一个账号级 session。
- 派生后的 thread 与收敛 session 不同，但改写过程没有建立这些 thread 与根线程之间的真实关系。
- 原文指出缺失的关系包括 `parent_thread_id`、`parent_turn_id`、`root_turn_id` 等 lineage。
- Bug 在于线程归属变化后，缺少与该归属匹配的父级和根级关系。

## 5. Compact 整体跳过身份处理，导致与普通请求不连续

- 原实现跳过 Compact 的账号身份 body 映射和指纹快照解析，且 Compact 白名单删除 `prompt_cache_key`。
- 当前修改：`off` / `device` 在请求改写前生成一次 canonical identity snapshot。普通 Responses、WebSocket 和 Compact 都从这份 snapshot 投影；Compact 只省略协议不支持的 `client_metadata` 和 `x-client-request-id`，继续携带相同的 installation/session/thread/window/cache identity。
- `prompt_cache_key` 已加入 Compact 白名单。默认缓存键引用原始 session 时，与 session 使用同一次映射；显式自定义缓存键保留独立缓存域。
- 状态：`off` / `device` 已修复、测试通过并部署。

## 6. HTTP、WebSocket 与 Compact 由不同 helper 分别改写

- 原实现让 HTTP header、WS handshake、WS frame body 和 Compact 分别读取、改写身份，可能从同一请求产生多套结果。
- WebSocket 的 device 模式尤其可能只改握手 installation，帧体仍保留另一套 installation。
- 当前修改：`off` / `device` 每次上游请求只解析一次 canonical snapshot，再分别投影到普通 body、HTTP/WS compatibility headers 和 Compact body。
- WS 首帧及后续 `response.create` / `session.update` 都在构造或更新握手头之前刷新 snapshot；握手与对应帧体不再独立派生身份。
- 状态：`off` / `device` 已修复、测试通过并部署。

## 7. transformed 与 passthrough 路径的身份规则不同

- 原实现中，结构化请求与 raw passthrough 请求经过不同 helper，字段优先级和 device installation 注入行为可能不同。
- 当前修改：两条路径共享同一 snapshot；map 与 raw projector 只负责不同的 JSON 写入方式，身份解析和映射结果相同。
- 状态：`off` / `device` 已修复、map/raw 等价用例通过并部署。

## 8. malformed metadata 会留下部分新身份和部分旧身份

- 原实现可能先改写平铺字段或 header，再保留无法解析的 `x-codex-turn-metadata`，形成两套冲突身份。
- 当前修改：嵌入式 metadata 无法解析时默认删除该 projection；不根据未知内容猜测 lineage、window state 或其他字段。合法 metadata 仍保留未知字段，只替换已确认的 identity 引用。
- 整个 `client_metadata` 不是对象时同样省略，不做半改写。
- 状态：已修复、测试通过并部署。

## 9. retry / failover 可能复用上一账号的身份快照

- Gin context 会在账号重试之间复用；若 staged identity 未清理，Account B 可能带着 Account A 的 installation/session/cache 映射出站。
- 当前修改：每次选择账号时清空旧 snapshot、指纹 IDs 和原始 cache source，再按当前 credential namespace 重新计算；staged snapshot 也校验当前 account ID。
- 状态：已修复、测试通过并部署。

## 10. resume 与窗口推进可能被无状态重写打断

- `off` / `device` 应继续客户端已有的 session/thread/turn/window graph；`device` 只替换 installation。
- 当前修改：账号隔离映射对相同原始 identity 保持确定性；`window_id = <thread_id>:<window_number>` 保留窗口序号，`context_window_id`、`first_window_id`、`previous_window_id` 分别稳定映射并维持引用关系。
- 状态：`off` / `device` 已修复并通过 resume/window progression 用例；`session` / `full` 的状态机问题仍属于第 2 至 4 项。

## 11. 账号隔离映射把 UUIDv7 identity 变成 UUIDv4 形状

- 状态：已知未修，后续需要单独设计。
- 当前账号隔离映射使用 `deriveStableUUIDv4()`，因此 session、thread、turn 以及独立 window identity 在映射后会变成 UUIDv4 形状。
- Codex 的这些有生命周期语义的 identity 通常由 UUIDv7 产生；UUIDv7 的前 48 bit 是时间戳，不能把 SHA-256 的随机前缀直接改一个 version bit 后伪装成合法 UUIDv7。
- 正确修法需要保留原 UUIDv7 的合法时间语义，或持久化一套真实的 UUIDv7 映射，不能只把派生函数从 v4 标签改成 v7 标签。
- installation identity 使用持久化随机 UUIDv4 是官方 Codex 的正常行为，不属于本项 bug。

## 今天实际修复了什么

### Identity graph 的引用关系

- 根线程原始满足 `session_id == thread_id == x-client-request-id` 时，账号隔离映射后仍保持三者相等。
- `prompt_cache_key` 原本引用 session 时，映射后继续等于 session；显式 override 的 cache key 保持独立缓存域。
- 原本不同的子线程 ID 不会因为根线程规则而合并。
- `parent_thread_id`、`x-codex-parent-thread-id`、`forked_from_thread_id` 引用 thread 节点；`parent_turn_id`、`root_turn_id` 引用 turn 节点；`first_window_id`、`previous_window_id`、`context_window_id` 引用对应 window 节点。
- `x-client-request-id` 是 thread identity 的投影，不再单独创建 `kind:request` identity。

### 一份 snapshot 投影所有载体

- `prepareCodexIdentitySnapshot()` 每个选定账号、每次出站 attempt 只解析一次身份。
- 输入优先级为：body 嵌入式 metadata、body 平铺字段、header 嵌入式 metadata、header 平铺字段。
- 同一 snapshot 投影到普通 Responses body、HTTP 兼容头、WS handshake、WS frame body 和 Compact。
- transformed 与 raw passthrough 共享同一解析结果；map/raw helper 只承担 JSON 写入。
- WS handshake 与 `response.create` / `session.update` 帧不再分别生成 installation/session/thread/window。
- Compact 延续普通 turn 的 installation/session/thread/window/cache；不虚构该协议不支持的 `client_metadata`。
- Messages 兼容桥仍按既有协议删除 body 中的 `prompt_cache_key`，但构造 session header 时可以使用原始 cache source。这个例外有回归测试，不应误报为 Compact/cache 回归。

### Device、异常 metadata 与账号切换

- `device` 只统一 installation，客户端真实的 session/thread/turn/window graph 继续存在。
- device installation 同时写入适用 header、平铺 `client_metadata` 与合法的嵌入式 turn metadata。
- 嵌入式 `x-codex-turn-metadata` 无法解析时默认删除该 projection，不部分改写，也不凭空最小重建未知 lineage/window 状态。
- 合法 metadata 会保留非 identity 字段，只替换确认过的引用。
- retry、failover 和 credential shadow 切换账号时，会清理并按新账号重建 snapshot，不能复用上一个账号的映射。

## 后续 agent 的代码入口

主要实现：

- `backend/internal/service/openai_codex_account_identity.go`：账号 namespace、关系感知映射、canonical snapshot 和各载体 projector。
- `backend/internal/service/openai_codex_fingerprint.go`：`device/session/full` 的指纹 ID 解析和写入 helper。
- `backend/internal/service/openai_gateway_forward.go`、`openai_gateway_passthrough.go`、`openai_gateway_request_body.go`：HTTP transformed/raw/Compact 请求流水线。
- `backend/internal/service/openai_gateway_messages.go`、`openai_gateway_chat_completions.go`：兼容协议桥的 cache/session 行为。
- `backend/internal/service/openai_ws_forwarder_payload.go`、`openai_ws_forwarder_ingress.go`、`openai_ws_v2_passthrough_adapter.go`、`openai_ws_forwarder_v2.go`：WS frame、握手与重连投影。

主要回归测试：

- `backend/internal/service/openai_codex_account_identity_graph_test.go`：根线程关系、lineage、cache、跨载体 canonical snapshot、device、malformed metadata、failover、resume/window progression。
- `backend/internal/service/openai_oauth_passthrough_test.go`：HTTP transformed/raw、Compact、Messages bridge。
- `backend/internal/service/openai_ws_forwarder_success_test.go`：WS handshake/body parity 与默认 cache key。
- `backend/internal/service/openai_codex_fingerprint_test.go`：各模式 helper、malformed metadata、map/raw 等价和 staged account 隔离。
- `backend/internal/service/openai_compact_service_tier_test.go`：Compact identity continuity。

## 后续审计方法

官方源码的已审计基线为 commit `c6058ccaa91ab17159cf805bf4d6d4edd87fe5fc`。本地参考源码位于 `codex/`，优先查看：

- `codex/codex-rs/core/src/responses_metadata.rs`
- `codex/codex-rs/core/src/client.rs`
- `codex/codex-rs/codex-api/src/endpoint/responses.rs`

如果本地 Codex 已更新，先比较新版本与上述 commit 的 metadata、window 和请求构造变化，再更新事实基线。不要把旧 issue 的描述直接当成当前协议事实。

寻找新问题时，按同一个逻辑 turn 对照以下路径：

1. HTTP transformed 与 raw passthrough。
2. WS handshake、第一帧、后续帧、连接复用和重连。
3. native compact 与 legacy compact/SSE bridge。
4. Account A 失败后切换 Account B，以及 credential shadow。
5. metadata 缺失、字段冲突、嵌入式 JSON 损坏、`client_metadata` 类型错误。
6. 根线程、subagent/fork、compaction、resume 和跨进程恢复。

审计重点是引用关系，不只是单个值是否为合法 UUID：

```text
root: session_id == thread_id
default cache: prompt_cache_key == session_id
request projection: x-client-request-id == thread_id
child thread: thread_id != root thread_id，并且 lineage 指回已有节点
device: 只替换 installation
compact: session/thread/cache continuity 不变，window 只按真实生命周期推进
all carriers: 同一次 attempt 使用同一 snapshot
```

报告一个新 bug 前，至少记录：可由官方 Codex 产生的原始字段组合、Sub2API 实际出站组合、断裂的 identity 关系、发生的载体/路径、当前账号模式，以及能稳定复现的测试。只看到值发生账号 namespace 映射，不等于 identity graph 发生了错误。

## 不要重复报告的边界

- `off` 下存在有效 account namespace 时继续做账号隔离映射，是有意行为，不是“off 未完全透传”的 bug。只有映射破坏关系或不同载体不一致时才算 bug。
- `session` / `full` 的 window、lineage、并发 ownership 和 resume 状态机问题是已知未修项。
- UUIDv7 identity 被映射成 UUIDv4 形状是第 11 项已知问题。
- 运行时额度、限流或缓存命中变化只能作为观察证据，不能单独证明 identity 实现错误。

## 简要列表

1. 根线程的 session/thread/request/cache 引用被不同 kind 拆散：`off` / `device` 已修复、验证并部署。
2. `session` / `full` 把窗口固定为 `:0`，丢失压缩后的窗口推进：未处理。
3. `session` / `full` 混用新 thread/window 与旧 window/context 状态：未处理。
4. `session` 模式重归属 thread 后缺少 parent/root lineage：未处理。
5. Compact 跳过 identity，导致普通请求和压缩请求断裂：`off` / `device` 已修复、验证并部署。
6. HTTP、WS handshake、WS body 各自改写，可能产生不同身份：`off` / `device` 已修复、验证并部署。
7. transformed 与 passthrough 路径身份规则漂移：`off` / `device` 已修复、验证并部署。
8. malformed metadata 被部分改写后继续透传：已改为默认删除坏 projection，已验证并部署。
9. retry / failover 复用上一账号 snapshot：已修复、验证并部署。
10. resume/window progression 在 `off` / `device` 下断裂：已修复并通过回归测试。
11. UUIDv7 identity 被账号隔离映射成 UUIDv4 形状：已知未修。
