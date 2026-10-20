# IM Agent

IM Agent 是一个本地优先的 macOS 桌面客户端，用于把 IM 消息安全地桥接到本机 Codex。项目使用 Go 与 Wails 构建，支持飞书桌面会话桥接，并新增钉钉群问答接入。两个渠道分别管理会话和执行方式。

> 当前仍是开发预览项目，不包含模型账号、Codex 订阅、IM 机器人凭证或第三方服务代理。部署者必须使用自己的 Codex 与所选渠道的企业内部应用。

## 核心能力

- macOS 客户端与独立后台：关闭客户端窗口不会停止后台，后台由本机控制接口和进程锁保护。
- 钉钉群问答：Stream 接收本企业白名单成员的群内 @消息，直接提问创建独立问答，引用回答续聊；后台 CLI 临时会话不进入 Codex 桌面历史。复用本机模型与认证；按统一业务范围回答，关闭任意主机操作和外部连接器。需求与 Bug 使用 grill-me 澄清后生成中文文档。
- 飞书长连接：接收白名单用户消息、交互卡片，发送文本、Markdown、卡片与附件。
- 会话管理：支持独立群和共享群话题，保存会话映射、历史游标、投递账本与状态卡消息 ID。
- 会话状态：`/status` 同时识别 IM 请求和桌面发起的轮次；桌面轮次按核验日志显示进度及结束状态，日志不足时显示待确认，不沿用旧任务结果。
- Codex 集成：默认跟随本机 `~/.codex` 配置，不接管模型账号；也可在高级模式指定兼容 CLI 或配置目录。
- 队列与回执：限制并发、网络请求时长和消息长度；对已提交、等待执行、完成、失败和送达不确定等状态分开记录。
- 本地凭证保护：飞书 App Secret 等凭证使用保存在系统钥匙串中的随机主密钥加密保存到本机 SQLite，不在界面回显，也不通过环境变量自动注入。
- 会话控制卡：过程输出只在卡片中更新，轮次结束清空预览，最终回复以聊天消息保留；支持下一轮模型选择和按真实轮次中断。
- 桌面命令审批：在绑定群或话题展示单次允许与拒绝入口；用户交互不设等待有效期，旧请求仍按真实生命周期失效。
- Alfred 辅助入口：提供打开客户端或只启动后台的固定入口，不依赖 Spotlight 索引。

## 架构概览

```text
飞书长连接 → internal/channels/feishu → internal/bridge → Codex 桌面 / CLI
钉钉 Stream → internal/channels/dingtalk → internal/qa → 独立 Codex CLI
                                         ↓
                              本地问答、引用映射与投递记录
```

桌面界面由 `frontend/` 与 Wails 绑定组成；后台控制面只监听本机 Unix Domain Socket，并使用本机 token 鉴权。

## 环境要求

- macOS Apple Silicon；当前项目重点在 macOS 15 附近验证，未承诺全平台兼容。
- Go 1.24 或更高版本。
- Wails CLI v2.12.0。
- macOS Command Line Tools。
- 已安装并可用、已登录的 Codex；IM Agent 不配置模型认证。
- 飞书渠道：由你控制的飞书自建应用，具备消息、卡片、群/话题和附件所需权限。
- 钉钉渠道：企业内部应用机器人，启用 Stream，具备群消息发送权限；CLI 需支持明确返回 `ephemeral=true`、`path=null` 的临时会话。钉钉接入尚未经过真实应用联调。

## 首次使用与 AI 引导

首次使用请按 [首次配置指南](docs/GETTING_STARTED.md) 逐步确认环境、飞书应用、桌面入口和第一条回复。希望由 AI 帮助时，复制 [AI 配置引导词](docs/AI_SETUP.md) 给它；每次只执行当前一步，密钥只在本机填写。

支持范围：当前以 macOS Apple Silicon 为主要目标；没有经过全新机器验证的 Codex 版本兼容矩阵。CLI 被发现、飞书已连接、桌面入口可用、模型完成、回复送达是不同状态，只有实际完成首次收发才能确认本机可用。

## 快速开始

```bash
git clone https://github.com/DrmBro/im-agent-open-source.git
cd im-agent-open-source

go version
go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0
export PATH="$(go env GOPATH)/bin:$PATH"
wails doctor

GOFLAGS=-trimpath wails build -clean
bash scripts/install-macos.sh build/bin/im-agent.app /Applications
```

安装后打开“IM Agent”，不要长期从 `build/bin/` 运行正式实例。

首次配置建议顺序：

1. 在“Codex 配置”选择“跟随本机 Codex”，点击重新检测，确认已发现 CLI 和用户配置。
2. 选择任务提交方式。桌面模式按会话 ID 提交，只连接本机回环地址的 Codex 桌面入口；不要将调试端口暴露到局域网或公网。
3. 在“IM 连接”添加飞书应用，填写 App ID 与 App Secret，并配置以 `ou_` 开头的用户白名单和管理员。
4. 保存配置后，在概览启动桥接，确认后台与飞书状态正常。
5. 使用无敏感内容的测试会话发送 `/status` 或 `/new`，再执行一个低风险任务。

`config.json.example` 仅用于旧 JSON 配置导入预览；运行时不从明文 `config.json` 或环境变量读取 IM 凭证。

## 钉钉业务问答

钉钉连接使用企业内部应用机器人，凭证为 Client ID / AppKey、Client Secret / AppSecret、RobotCode。白名单与管理员同时为空时为身份识别模式，群内 `@机器人 /whoami` 仅返回本人企业 UserID，不自动授权；普通问题不执行。

问答模式要求配置人员和业务范围说明。所有白名单成员，包括管理员，采用相同业务边界；范围为空时不执行普通问题。业务资料可显式选择最多 32 个 UTF-8 文件，总计最多 64000 字符，每文件最多 256 KiB；程序不会扫描主机目录、读取未配置的个人文件或执行系统命令。业务模式复用本机模型和认证，关闭 Shell、外部 MCP 连接器、网页搜索、自动 hooks、通知命令与技能依赖安装；不修改个人 Codex 配置。

直接 @新开独立问答；引用回答或接收回执并 @可续聊。每轮先检查业务相关性，生成后再复核回复范围；“这台电脑在做什么”等宿主机查询拒绝，不因添加“需求”或“Bug”前缀而放行。敏感信息脱敏和按人配额不属于本次实现。

业务需求与业务 Bug 显式加载本机实际可用的 `grill-me` 和 `grilling`。信息不足时在群内追问，信息齐备后生成中文 Markdown。需求文档包含目标、流程、边界、实施建议与验收；Bug 文档区分已确认事实与待验证假设。文档由程序写入应用数据目录并建立 SHA256 索引，群内返回文档内容，本机“任务与附件”支持按需查看和复制。

问题持久化后产生接收回执，关键处理阶段和长时间等待有进度消息。引用某轮回执或回答并 @发送 `/status`、`进度` 或 `进度查询`，查询该轮真实进度，不调用模型，也不要求填写编号。阶段记录标明采集时刻，过期排队/等待通知在发送前取消；接收回执与最终回答使用独立队列。模型已回复和需求事项完成分别记录，澄清问题发送后事项仍处于澄清阶段。

模型事件、网络与提供方错误、审批、补充等待、文档保存和回答投递分别记录。最后模型事件时间只由实际 CLI 事件更新，不用心跳或连接正常伪装处理进展，不显示虚构百分比。发送成功表示平台已接受请求，不代表群成员已读。

CLI 会话为独立进程内的临时会话，不进入 Codex 桌面历史。后台重启或空闲回收后，从最近最多 30 轮、约 64000 字符的已完成问答重建上下文，不恢复工具中间状态。未知任务停止续接且不自动重跑，发送结果未知须核对；移除连接不删除其历史账本。当前接收文本，输出文本 / Markdown，暂不包含钉钉附件、交互卡片或自动建群。

## 配置说明

配置保存在本机 `desktop.sqlite`；飞书凭证另存为本机加密数据，主密钥保存在系统钥匙串。保存配置后需要停止桥接并重新启动，后台不会热替换运行中的配置。

### 推荐最小配置

| 页面 | 必填项 | 推荐值 |
| --- | --- | --- |
| Codex 配置 | 无需模型凭证 | `mode=follow_local`，`submission_mode=native_queue` |
| IM 连接 | App ID、App Secret、用户白名单、管理员 | 白名单使用 `ou_` 开头的飞书 Open ID |
| 会话模式 | 可选 | 新用户先用 `group`；确认共享群阅读范围后再用 `topic` |
| 沙箱与审批 | 必选 | `sandbox=workspace-write`，`approval_policy=untrusted` |
| 会话同步 | 可选 | `watch.enabled=false`，确认风险后再开启 |

### 关键配置约束

- `allowed_users`：1–500 个飞书 Open ID；不在白名单内的用户会被忽略。
- `admin_users`：1–49 个，且必须全部同时位于 `allowed_users`。
- `conversation_mode`：`group` 或 `topic`；只影响新建或尚未绑定的会话。
- `shared_chat_id`：仅 `topic` 模式使用，必须是机器人已加入的 `oc_` 开头群 ID。
- `sandbox`：只允许 `read-only` 或 `workspace-write`。
- `approval_policy`：只允许 `untrusted` 或 `on-request`。
- `watch`：默认关闭；目标连接必须开启 `auto_create_chat`，扫描间隔为 5–3600 秒。
- `cdp_port`：`0` 表示自动探测；非零值是首选本机 CDP 端口。桌面入口不得暴露到局域网或公网。

字段级默认值、取值范围、导入行为和常见错误见 [docs/CONFIGURATION.md](docs/CONFIGURATION.md)。

## 安全边界

- 只允许显式白名单用户使用；管理员必须同时位于白名单内。
- Codex 沙箱仅允许 `read-only` 或 `workspace-write`，不允许无沙箱模式。
- 桌面 CDP 入口必须保持本机回环访问；不要开放远程调试端口。
- 飞书 App Secret、Verification Token、Encrypt Key 只保存在本机；备份配置数据库时必须连同系统钥匙串迁移计划一起考虑，不要直接发送数据库。
- 任务“已提交但结果或送达未确认”时不要重发；未知状态可能已经在桌面执行。
- 草稿自动恢复是实验能力，默认关闭；重要草稿不应依赖它。

完整部署、升级、命令行运行和打包流程见 [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)。

## 发布与打包

源码构建输出位于忽略目录 `build/bin/`。生成可分发 DMG 前，先使用干净快照和 `GOFLAGS=-trimpath` 构建，再用：

```bash
go mod download -json > go-mod-download.json
python3 scripts/collect-go-notices.py \
  build/bin/im-agent.app/Contents/MacOS/im-agent \
  go-mod-download.json \
  "$(go env GOROOT)/LICENSE" \
  THIRD_PARTY_NOTICES.txt

bash scripts/package-macos.sh \
  build/bin/im-agent.app \
  THIRD_PARTY_NOTICES.txt \
  dist \
  open-source-preview
```

当前打包脚本生成 ad-hoc 签名且未公证的 Apple Silicon DMG，并会检查发行白名单与开发者主目录路径。公开发布仍应准备正式签名与公证；不要指导用户关闭 Gatekeeper。

## 迭代记录

功能与兼容性变化见 [更新记录](CHANGELOG.md)。尚未正式发布的变化记录在“未发布”，不把本机安装等同于公开发行。

## 贡献

提交范围、消息格式和验证要求见 [贡献指南](CONTRIBUTING.md)。

提交前请检查：

- 不提交 `config.json`、数据库、钥匙串导出、日志、会话记录、附件、构建产物或本地临时目录。
- 不在 issue、截图、录屏或文档中粘贴 App Secret、访问令牌、真实消息 ID、群 ID、Open ID 或业务内容。
- 代码与文档使用中文描述业务语义；安全提示不夸大未验证的状态。

## 许可证

本项目基于 [MIT License](LICENSE) 发布。
