# IM Agent

IM Agent 是一个本地优先的 macOS 桌面客户端，用于把 IM 消息安全地桥接到本机 Codex。项目使用 Go 与 Wails 构建，当前首发渠道是飞书；IM 适配层已从桥接核心隔离，便于后续扩展其他渠道。

> 当前仍是开发预览项目，不包含模型账号、Codex 订阅、飞书机器人凭证或第三方服务代理。部署者必须使用自己的 Codex 与飞书应用。

## 核心能力

增加 [钉钉个人账号（DWS）](docs/DINGTALK_PERSONAL.md)：本人授权后，对指定白名单联系人的单聊生成处理判断与待审回复，通过桌面通知和“消息助理”页确认后发送；自动回复需按联系人另行开启。与应用机器人分开配置，首次使用需要 DWS 登录及联系人授权。

新增钉钉独立 CLI 接入源码：飞书保持原桌面路径，钉钉私聊／已有群通过独立 Codex App Server 执行。当前未构建和联调，能力与限制见 [钉钉与独立 Codex CLI](docs/DINGTALK_CLI.md)。

- 消息助理：定时拉取今天关注的人和群的钉钉消息，用你在 CC Switch 中配置的模型按事项拆分，分为“需要我处理 / 可自动回复 / 无需回复”；复杂事项派发独立 Codex 会话，后续新消息进入队列由会话自取。详见 [钉钉个人账号](docs/DINGTALK_PERSONAL.md#消息助理父-agent-分流)。

- macOS 客户端与独立后台：关闭客户端窗口不会停止后台，后台由本机控制接口和进程锁保护。
- 飞书长连接：接收白名单用户消息、交互卡片，发送文本、Markdown、卡片与附件。
- 同步诊断：会话同步页分别展示日志读取进度、已确认送达消息、待发送与未知回执、重试时间和摘要状态；概览同时显示审批能力状态与实际运行版本。
- 会话管理：支持独立群和共享群话题，保存会话映射、历史游标、投递账本与状态卡消息 ID。
- Codex 集成：默认跟随本机 `~/.codex` 配置，不接管模型账号；也可在高级模式指定兼容 CLI 或配置目录。
- 队列与回执：限制并发、网络请求时长和消息长度；对已提交、等待执行、完成、失败和送达不确定等状态分开记录。
- 恢复边界：看板、历史任务和请求卡共用状态规则；重启仅恢复明确未提交的输入，撤权或绑定失效的旧输入不自动复活。详细行为见 [远程状态与恢复边界](docs/REMOTE_STATE_RECOVERY.md)。
- 本地凭证保护：飞书 App Secret 等凭证使用保存在系统钥匙串中的随机主密钥加密保存到本机 SQLite，不在界面回显，也不通过环境变量自动注入。
- 会话控制卡：过程输出只在卡片中更新，轮次结束清空预览，最终回复以聊天消息保留；支持下一轮模型选择和按真实轮次中断。
- 桌面命令审批：在绑定群或话题展示单次允许与拒绝入口；用户交互不设等待有效期，旧请求仍按真实生命周期失效。
- 桌面 Question 提问：兼容原生待回答请求与新版异步提问，轮次完成后仍可在绑定群或话题回答；支持选项、自定义回答、多题一次发送及整组跳过，自定义回答优先，每题最多 1000 字。涉密问题及超出卡片展示范围的问题仍需在桌面回答。
- Alfred 辅助入口：提供打开客户端或只启动后台的固定入口，不依赖 Spotlight 索引。

## 架构概览

```text
飞书长连接
   ↓
internal/channels/feishu  → internal/im 适配层
   ↓
internal/bridge（队列、会话、投递、状态卡、附件）
   ↓
internal/codex（Codex App Server / 桌面输入集成）
   ↓
本机 Codex CLI / Codex 桌面端
```

桌面界面由 `frontend/` 与 Wails 绑定组成；后台控制面只监听本机 Unix Domain Socket，并使用本机 token 鉴权。

## 环境要求

- macOS Apple Silicon；当前项目重点在 macOS 15 附近验证，未承诺全平台兼容。
- Go 1.24 或更高版本。
- Wails CLI v2.12.0。
- macOS Command Line Tools。
- 已安装并可用、已登录的 Codex；IM Agent 不配置模型认证。
- 一个由你控制的飞书自建应用，具备消息、卡片、群/话题和附件所需权限。

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
- 后台辅助进程的 `sandbox` 允许 `read-only` 或 `workspace-write`；飞书新建和分叉会话明确使用 Full Access（`danger-full-access`、`never`），创建时核对实际权限。
- `approval_policy`：只允许 `untrusted` 或 `on-request`。
- `watch`：默认关闭；目标连接必须开启 `auto_create_chat`，扫描间隔为 5–3600 秒。
- `cdp_port`：`0` 表示自动探测；非零值是首选本机 CDP 端口。桌面入口不得暴露到局域网或公网。

字段级默认值、取值范围、导入行为和常见错误见 [docs/CONFIGURATION.md](docs/CONFIGURATION.md)。

## 安全边界

- 只允许显式白名单用户使用；管理员必须同时位于白名单内。
- 飞书新建会话使用 Full Access，命令无需逐次审批；后台辅助进程的沙箱配置独立，恢复已有会话保留其自身权限。
- 桌面 CDP 入口必须保持本机回环访问；不要开放远程调试端口。
- 飞书 App Secret、Verification Token、Encrypt Key 只保存在本机；备份配置数据库时必须连同系统钥匙串迁移计划一起考虑，不要直接发送数据库。
- 任务“已提交但结果或送达未确认”时不要重发；未知状态可能已经在桌面执行。
- 正式桌面提交按会话 ID 调用，不操作输入框；旧版已保存的草稿仍可在本机查看和导出。

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

## 本机同步维护

消息摘要和会话状态共用增量日志观察器；逐条消息使用相同的用户消息识别和最终回复核验规则。读取进度与飞书送达回执分别持久化，明确失败按有上限退避重试，未知送达保留待核对。同步按会话隔离执行，历史异常不会清空后续消息的账本。

`scripts/release-local.py` 提供完整工作区的 `build`、发布清单 `verify` 和原位更新 `deploy` 三个动作。每份产物包含完整源码清单及 SHA-256；后台 `/status` 的 `build.source_digest` 必须与安装清单一致。该脚本用于现有本机安装的升级，不替代首次配置。安装与更新仅在切换程序时临时保留原文件，结束后清理，部署不生成数据库快照。

`scripts/inspect-sync.py` 只读汇总运行版本、逐会话同步状态和回执；`--thread` 可核对指定会话的消息标识及审批记录。每个发布目录中的 `sync-inspect` 只读取本机会话日志，输出最新用户/助手条目标识、完成证据和状态，不输出正文，不执行模型任务。

验收边界：运行版本、现有真实日志、数据库回执、审批目录和重启恢复可直接核对。断网、进程异常退出、平台超时等故障注入需要专门安排，不能用构建成功代替这些场景的验收。

关于 Codex 桥接上游项目的实现比较、采用范围和兼容限制，见 [桥接项目调研与实现借鉴](docs/CODEX_BRIDGE_UPSTREAMS.md)。源码级 MIT 借鉴及版权声明保存在 `third_party/happy/`，并随本机发布和许可证汇总分发。

## 分支约定

后续所有开发、修复、配置和文档改动统一在 `main` 分支进行。提交前先确认当前分支为 `main`；不再从本仓库创建长期功能分支。已有分支需要合并时，先核对与 `main` 的提交差异和冲突，再合并并验证。
