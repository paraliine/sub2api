# Sub2API Codex Bug 清单

本文记录原文中的具体实现错误及后续核查进展。

## 1. 身份映射破坏同一实体的引用关系

- 状态：账号隔离映射层代码已修改，回归用例已补充，按要求暂缓执行测试。
- 触发场景：对普通根线程进行账号身份映射，原始请求满足 `session_id == thread_id == x-client-request-id`。
- 原实现分别使用 `kind:session`、`kind:thread`、`kind:request` 派生 ID，即使原始值相同，也会得到不同结果。
- `x-client-request-id` 在该场景中是 thread identity 的引用，单独派生 request identity 会破坏这一关系。
- Bug 在于映射后未保持原有的实体引用关系。
- 当前修改：session/thread/request 及 parent/fork thread 引用共用 session 域；turn 和窗口历史引用分别跟随 turn/window 域。installation 保持独立域。默认缓存键从原始 Header、平铺或嵌套 metadata 识别 session 引用。
- 本项不包含后续 `session/full` 收敛策略对身份的覆盖。

原问题示例：

```text
原始：session_id = S1，thread_id = S1，x-client-request-id = S1
改写：session_id = S'，thread_id = T'，x-client-request-id = R'
结果：原本相等的引用被拆成不同 ID
```

## 2. 窗口标识固定，压缩后无法切换到新窗口

- 本地 Codex 的 `current_window()` 实际返回 `<thread_id>:<window_number>`，独立 UUID 为 `context_window_id`。拼接格式本身不列为 bug。
- 正常压缩后，线程 ID 保持不变，当前窗口 ID 发生变化，`window_number` 递增。
- `session` / `full` 固定生成 `<thread_id>:0`，同一线程压缩前后得到相同的窗口标识。
- Bug 在于不同压缩窗口被映射为同一个 window，窗口切换信息丢失。
- 硬编码的是拼接结果中的 `:0`；`window_number` 仍可能保留客户端的旧值。

## 3. 部分字段改写导致窗口 metadata 状态不一致

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

- 多个无关客户端 session 被收敛到同一个账号级 session。
- 派生后的 thread 与收敛 session 不同，但改写过程没有建立这些 thread 与根线程之间的真实关系。
- 原文指出缺失的关系包括 `parent_thread_id`、`parent_turn_id`、`root_turn_id` 等 lineage。
- Bug 在于线程归属变化后，缺少与该归属匹配的父级和根级关系。

## 5. Compact 整体跳过身份处理，导致与普通请求不连续

- 原实现跳过 Compact 的账号身份 body 映射和指纹快照解析，且 Compact 白名单删除 `prompt_cache_key`。
- 当前修改：`off` / `device` 在请求改写前生成一次 canonical identity snapshot。普通 Responses、WebSocket 和 Compact 都从这份 snapshot 投影；Compact 只省略协议不支持的 `client_metadata` 和 `x-client-request-id`，继续携带相同的 installation/session/thread/window/cache identity。
- `prompt_cache_key` 已加入 Compact 白名单。默认缓存键引用原始 session 时，与 session 使用同一次映射；显式自定义缓存键保留独立缓存域。
- 状态：代码和回归用例已修改，按要求暂缓执行测试。

## 6. HTTP、WebSocket 与 Compact 由不同 helper 分别改写

- 原实现让 HTTP header、WS handshake、WS frame body 和 Compact 分别读取、改写身份，可能从同一请求产生多套结果。
- WebSocket 的 device 模式尤其可能只改握手 installation，帧体仍保留另一套 installation。
- 当前修改：`off` / `device` 每次上游请求只解析一次 canonical snapshot，再分别投影到普通 body、HTTP/WS compatibility headers 和 Compact body。
- WS 首帧及后续 `response.create` / `session.update` 都在构造或更新握手头之前刷新 snapshot；握手与对应帧体不再独立派生身份。
- 状态：代码和回归用例已修改，按要求暂缓执行测试。

## 7. transformed 与 passthrough 路径的身份规则不同

- 原实现中，结构化请求与 raw passthrough 请求经过不同 helper，字段优先级和 device installation 注入行为可能不同。
- 当前修改：两条路径共享同一 snapshot；map 与 raw projector 只负责不同的 JSON 写入方式，身份解析和映射结果相同。
- 状态：代码和 map/raw 等价用例已补充，按要求暂缓执行测试。

## 8. malformed metadata 会留下部分新身份和部分旧身份

- 原实现可能先改写平铺字段或 header，再保留无法解析的 `x-codex-turn-metadata`，形成两套冲突身份。
- 当前修改：嵌入式 metadata 无法解析时默认删除该 projection；不根据未知内容猜测 lineage、window state 或其他字段。合法 metadata 仍保留未知字段，只替换已确认的 identity 引用。
- 整个 `client_metadata` 不是对象时同样省略，不做半改写。
- 状态：代码和回归用例已修改，按要求暂缓执行测试。

## 9. retry / failover 可能复用上一账号的身份快照

- Gin context 会在账号重试之间复用；若 staged identity 未清理，Account B 可能带着 Account A 的 installation/session/cache 映射出站。
- 当前修改：每次选择账号时清空旧 snapshot、指纹 IDs 和原始 cache source，再按当前 credential namespace 重新计算；staged snapshot 也校验当前 account ID。
- 状态：代码和回归用例已修改，按要求暂缓执行测试。

## 10. resume 与窗口推进可能被无状态重写打断

- `off` / `device` 应继续客户端已有的 session/thread/turn/window graph；`device` 只替换 installation。
- 当前修改：账号隔离映射对相同原始 identity 保持确定性；`window_id = <thread_id>:<window_number>` 保留窗口序号，`context_window_id`、`first_window_id`、`previous_window_id` 分别稳定映射并维持引用关系。
- 状态：`off` / `device` 代码和 resume/window progression 用例已修改；`session` / `full` 的状态机问题仍属于第 2 至 4 项。

## 简要列表

1. 根线程的 session/thread/request/cache 引用被不同 kind 拆散：`off` / `device` 已修改，待统一测试。
2. `session` / `full` 把窗口固定为 `:0`，丢失压缩后的窗口推进：未处理。
3. `session` / `full` 混用新 thread/window 与旧 window/context 状态：未处理。
4. `session` 模式重归属 thread 后缺少 parent/root lineage：未处理。
5. Compact 跳过 identity，导致普通请求和压缩请求断裂：`off` / `device` 已修改，待统一测试。
6. HTTP、WS handshake、WS body 各自改写，可能产生不同身份：`off` / `device` 已修改，待统一测试。
7. transformed 与 passthrough 路径身份规则漂移：`off` / `device` 已修改，待统一测试。
8. malformed metadata 被部分改写后继续透传：已改为默认删除坏 projection，待统一测试。
9. retry / failover 复用上一账号 snapshot：已修改，待统一测试。
10. resume/window progression 在 `off` / `device` 下断裂：已修改，待统一测试。
