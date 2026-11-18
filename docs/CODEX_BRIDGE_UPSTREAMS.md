# Codex 桥接项目调研与实现借鉴

核对日期：2026-10-28。以 GitHub API 仓库信息、固定提交的源码及本机只读协议结果为依据，不将 README 功能声明当作实际恢复能力。Star 数仅表示关注度，不能证明可靠性。

## 项目选择

| 项目 | 核对时关注度 | 维护证据 | 本次用途 |
| --- | ---: | --- | --- |
| [slopus/happy](https://github.com/slopus/happy) | 24,366 Star | 最近推送 2026-10-28；Codex 客户端、集成和恢复测试文件；MIT 许可文本已取得 | 主要参考：协议关联、轮次等待、会话队列、断线清理 |
| [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect) | 16,129 Star | 最近推送 2026-10-27；有独立 Codex 代理和飞书适配器 | 架构参考：IM 与模型代理隔离、审批转发、事件归属筛选。本次固定提交根目录未发现 LICENSE/LICENCE 文件，GitHub license 为 null，不搬入其源码 |
| [happy-loki/codexhub](https://github.com/happy-loki/codexhub) | 176 Star | 最近推送 2026-10-01；Apache-2.0 许可文本已取得 | 次级参考：多种通知格式、消息文本提取和审批选项映射 |
| [momolm/codex-mainline](https://github.com/momolm/codex-mainline) | 34 Star | 最近推送 2026-08-04；MIT；有 Telegram 输入缓冲、投递及不活跃观察相关测试文件 | 专项参考：图片/文字合并缓冲、流式文本按 item 保存、运行状态持久化 |

较小的飞书专用仓库也做了初筛，但没有把它们与 Happy、cc-connect 的维护规模等同。成功读取的源码保存在忽略目录 `.work/upstream-bridges-20261028/`。部分 git clone/原始文件下载失败后改用 GitHub API 取文件；没有安装或执行上游项目。

## 固定源码依据

- Happy：`85aa1baf5a786f0d4d0f6bb2b2883be47d51564b`
  - [codexAppServerClient.ts](https://github.com/slopus/happy/blob/85aa1baf5a786f0d4d0f6bb2b2883be47d51564b/packages/happy-cli/src/codex/codexAppServerClient.ts)：`request`、`handleLine`、`handleRawNotification`、`tryResolvePendingTurn`、`resumeThread`。
  - [runCodex.ts](https://github.com/slopus/happy/blob/85aa1baf5a786f0d4d0f6bb2b2883be47d51564b/packages/happy-cli/src/codex/runCodex.ts)：`MessageQueue2`、`sendTurnAndWait`、轮次结束后的 `flush`。
  - [emitReadyIfIdle.ts](https://github.com/slopus/happy/blob/85aa1baf5a786f0d4d0f6bb2b2883be47d51564b/packages/happy-cli/src/codex/emitReadyIfIdle.ts)：无待处理输入且队列为空时才发 ready。
- cc-connect：`b2a6e6e71cdfe07ebf96bb330e845e070900c02a`
  - [appserver_session.go](https://github.com/chenhg5/cc-connect/blob/b2a6e6e71cdfe07ebf96bb330e845e070900c02a/agent/codex/appserver_session.go)：连接和初始化、`ensureThread`、`acceptsTurnNotification`、`handleItemCompleted`、`completeTurn`、审批请求表。
  - [feishu.go](https://github.com/chenhg5/cc-connect/blob/b2a6e6e71cdfe07ebf96bb330e845e070900c02a/platform/feishu/feishu.go)：平台消息去重对象、消息回执及卡片上下文。
- codexhub：`9154e9cd4283f7b4f2142cee5f089c1f2b5e72fe`
  - [src/codex.rs](https://github.com/happy-loki/codexhub/blob/9154e9cd4283f7b4f2142cee5f089c1f2b5e72fe/src/codex.rs)：不同事件字段和消息文本提取、完成判断、审批决定映射。
- codex-mainline：`ceb2c5eedd784e80a29cddd9ce00f1f48a13aace`
  - [start-codex-mainline.mjs](https://github.com/momolm/codex-mainline/blob/ceb2c5eedd784e80a29cddd9ce00f1f48a13aace/src/start-codex-mainline.mjs)：WebSocket 初始化、`ensureThread`、按 item 累积 delta、轮次完成时更新状态。
  - `src/telegram-input-buffer.mjs`：按 Telegram message_id 放入 Map、文字与图片采用不同收集时间。
  - `src/telegram-reply-delivery.mjs`：富文本发送抛错后调用纯文本发送。

## 他们如何处理消息

### 接收和排队

Happy 把输入放入消息队列，再逐条执行 `sendTurnAndWait`。轮次结束不等于全部输入处理完毕，只有没有 pending 且队列为空才发送 ready。cc-connect 将飞书平台适配和 Codex session 分开，平台维护去重和回复上下文，代理输出统一事件。codex-mainline 将短时间内的文字、图片消息按原平台消息 ID 缓冲合并。

本项目已使用 SQLite 持久收件和投递账本。不能为了照搬内存 Map 而退化重启防重；可直接借鉴的是职责分离和“队列为空才就绪”的判断方式。

### 会话恢复与提交

Happy、cc-connect 都先 initialize，再按原 thread_id 恢复或创建会话。RPC 以请求 ID 关联响应；错误与超时有单独路径。Happy 的客户端还区分进程 epoch，防止旧进程迟到的退出/响应清理新连接。

它们通常自己管理 app-server 生命周期。本项目则要控制已有桌面会话并同步桌面输入，不能直接换成第二个自行启动的执行服务，否则可能失去桌面归属。近期 thread/resume 成功但桥接未确认回执，就是额外桌面转发层带来的问题。已完成的有限加载重查仍需保留。

### 流式输出和最终回复

Happy 处理原始 `item/agentMessage/delta`、`item/completed`、`turn/completed`，完成条目携带完整正文及 item_id。cc-connect 缓存已完成 agentMessage，在工具开始时可作为过程输出，在轮次结束时作为最终文本输出。codex-mainline 同时维护整体文本和按 item_id 的文本缓冲。

应采用的共同点：预览与最终正文分开；正文使用完整条目；身份关联优先使用原 thread/turn/item 标识。不能把“相同文字”当作相同消息，也不能把前面已展示的 delta 再拼到完整正文前。

### 结束、审批与重试

Happy 的 `tryResolvePendingTurn` 用真实 turn_id 排除另一轮的迟到完成；它不要求一定先观察到 turn/started，因为快速轮次可能先到终态。其原始事件处理还支持 idle 或 final_answer 驱动完成。cc-connect 也有 idle 驱动完成的分支。对于本项目，idle 可能是另一轮状态，final_answer 条目也不一定是整个任务终态，因此这两个快捷分支没有照搬。

审批以原 request_id 维护等待和响应，不能由普通文本模拟允许。连接失败时清理旧等待，不表示再次提交原任务。codex-mainline 的富文本失败后直接发纯文本也不能原样搬入：若失败属于网络超时、实际送达未知，第二次发送可能重复。

## 本轮已经落到本项目的代码

1. `internal/codex/protocol_identity.go`：基于 Happy MIT 实现改写轮次标识提取与目标轮次匹配，兼容 camelCase/snake_case。多个显式标识互相冲突时拒绝事件，比上游“取第一个非空值”更严格。
2. `internal/codex/events.go` 与 `internal/bridge/bridge.go`：统一协议身份解析，对另一会话/另一轮的迟到事件做过滤，涵盖失败通知及文本通知。
3. `internal/codex/desktop_protocol_observer.go`：桌面提交后按服务器返回的真实 turn_id 优先查询协议结果；只读轮询约 2 秒一次，最多 4 页寻找确切轮次，协议连续失败 3 次才切到原日志观察。这里采用轮询适配已有桌面宿主，并未复制上游自建 app-server 的实时订阅通道。
4. `internal/codex/desktop_session_control.js`：返回有限分页元数据和公开助手条目，保留原始 ID、阶段、状态与完整正文，不返回工具输出和推理条目。
5. `item/completed` 和完成通知按顺序送给现有桥接，完整正文继续进入同一投递账本；通知背压时可取消地等待，避免静默丢掉完成事件。
6. 保留 MIT 原许可及来源固定提交；本机发布与第三方许可汇总中包含 Happy 来源声明。

## 现场发现的关键兼容差异

只读检查三条真实桌面会话的轮次列表：其中一条的最终条目标识是原始 `msg_…`，且已存在投递账本；另外两条的最终条目标识出现 `item-29`、`item-60` 等重建 ID，账本中不存在。

因此新协议观察路径目前只接受已确认格式的 `msg_` 原始标识、明确 final_answer 和完成轮次；重建 ID、缺省阶段、条目不完整会回退日志核验。不能把重建 ID 当新消息补发，否则同一回复可能分别以日志 ID 和重建 ID 发送两遍。

## 尚未替换的部分及验收边界

- 自动镜像桌面发起的新输入、历史漏发恢复仍使用日志及持久游标；本轮改变的是飞书提交后的结果观察，不是将所有链路一次性换成 Happy。
- 桌面审批仍需要当前版本适配器。上游自建 app-server 的审批事件不能直接接管另一桌面宿主持有的请求。
- 保留 Full Access 新会话创建要求、现有会话绑定、已送达与未知回执。
- 完成源码阅读、JavaScript/Python 语法、Go 格式解析、静态引用及只读现场协议检查。按项目约定未新增测试文件、未编译、未运行测试套件；尚未部署，也未用新观察器执行真实轮次。
