# IM Agent 配置参考

本文是字段级配置说明。推荐通过 IM Agent 客户端配置；客户端会把非敏感配置保存到本机 `desktop.sqlite`，把 App ID、App Secret 等凭证加密后保存到本机数据库，加密主密钥保存在当前用户钥匙串。

> 不要把 `config.json`、`desktop.sqlite`、钥匙串导出、日志或业务数据库提交到 Git，也不要发送给他人。`config.json.example` 只用于旧 JSON 导入预览，运行时不会从明文 JSON 或环境变量读取飞书凭证。

首次使用先阅读 [首次配置指南](GETTING_STARTED.md)，其中包含外部权限、版本兼容及首条任务验证；本文提供字段参考。

## 配置入口与生效方式

| 操作 | 位置 |
| --- | --- |
| 日常配置 | 客户端页面：`Codex 配置`、`IM 连接`、`运行限制`、`会话同步` |
| 旧 JSON 导入 | 客户端 `导入 JSON`，只生成预览草稿，仍需点击保存 |
| 生效方式 | 保存后需要停止桥接，再从概览重新启动桥接 |
| 凭证保存 | 界面输入后保存到本机加密表；不回显，留空表示保留旧值 |
| 删除连接 | 保存后会删除该连接的本地加密凭证；业务历史保留 |

旧 JSON 导入有以下限制：

- 文件不超过 1 MB。
- 不接受未知字段。
- 不支持明文凭证字段。
- 旧版 `app_id_env`、`app_secret_env`、`verification_token_env`、`encrypt_key_env` 会被忽略。
- 导入只替换界面草稿；必须点击保存才写入 SQLite。

## 顶层配置

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `work_dir` | string | 空 | 可选的新会话默认项目目录。留空时由每个新会话自动分配目录。必须是已存在的绝对路径；支持 `~/`。已绑定会话不会因此迁移。 |
| `state_dir` | string | 由客户端设置 | 运行时业务状态目录。桌面客户端会将其固定为应用数据目录下的 `runtime/`，普通部署不需要手工修改。 |
| `codex` | object | 见下文 | Codex 集成配置。 |
| `connections` | array | 空数组 | IM 连接列表。启动桥接时至少需要 1 个连接；桌面客户端最多保存 20 个连接。 |
| `limits` | object | 见下文 | 队列、超时和消息长度限制。 |
| `watch` | object | 见下文 | 可选的 Codex 会话自动发现配置。 |

## Codex 配置

| 字段 | 类型 | 默认值 | 可选值 / 约束 | 说明 |
| --- | --- | --- | --- | --- |
| `mode` | string | `follow_local` | `follow_local`, `custom` | 默认跟随当前用户 `~/.codex`。`custom` 用于指定兼容 CLI 或配置目录。 |
| `submission_mode` | string | `native_queue` | `native_queue`, `direct`, `desktop_queue` | 推荐 `native_queue`，通过桌面所属连接按会话 ID 提交，不操作当前输入框。`direct` 表示 Codex App Server 直接提交。`desktop_queue` 是兼容值，不建议手工填写。 |
| `auto_switch_thread` | bool | `false` | `true`, `false` | 旧输入框兼容路径的导航设置。当前正式桌面提交按会话 ID 调用，不依赖此设置切换窗口。 |
| `preserve_desktop_draft` | bool | `false` | `true`, `false` | 旧输入框兼容路径的草稿保护设置。当前正式提交不读写输入框草稿；未知提交结果不会回退到输入框重发。 |
| `auto_restore_desktop_draft` | bool | `false` | `true`, `false` | 实验功能。仅在 `preserve_desktop_draft` 开启时才可能生效；默认必须保持关闭。 |
| `binary` | string | 空 | `custom` 模式可填 | Codex CLI 路径或可在 `PATH` 中找到的名称。留空时自动发现。`follow_local` 模式会清空该字段。 |
| `home` | string | 空 | `custom` 模式可填 | `CODEX_HOME` 目录。留空使用 `~/.codex`；必须是实际 Codex 配置目录。`follow_local` 模式会清空该字段。 |
| `model` | string | 空 | `custom` 模式可填 | 旧版兼容模型覆盖。推荐留空，让 Codex 使用本机配置。`follow_local` 模式会清空该字段。 |
| `sandbox` | string | `workspace-write` | `read-only`, `workspace-write` | 不允许无沙箱模式。 |
| `approval_policy` | string | `untrusted` | `untrusted`, `on-request` | 保存仅支持这两个值。若 CLI 帮助不再包含 `untrusted`，运行时收紧为 `on-request` + `read-only`，不修改已保存配置；详见首次配置指南。 |
| `cdp_port` | int | `0` | `0` 或有效本机端口号 | 桌面 CDP 首选端口。`0` 表示自动探测；程序还会探测本机 9222–9235。端口必须只监听回环地址。 |

## IM 连接配置

支持 `feishu` 与 `dingtalk`，分别使用桌面会话桥接和群内独立问答。每个连接的 `id` 保存后不可改名，因为凭证和会话历史都按连接 ID 关联。

### 连接公共字段

| 字段 | 类型 | 默认值 | 约束 | 说明 |
| --- | --- | --- | --- | --- |
| `id` | string | UI 自动生成 `im-1` 等 | 1–64 字符；首字符为字母或数字；只能包含字母、数字、`_`、`-`；必须唯一 | 连接唯一标识。保存后不可修改。 |
| `type` | string | UI 选择 | `feishu`, `dingtalk` | IM 渠道类型。保存后不可修改。 |
| `allowed_users` | string array | 飞书必填；钉钉可先留空 | 问答模式 1–500 个；钉钉识别模式允许与管理员同时为空；飞书使用 `ou_` 开头的 Open ID，钉钉使用企业用户 ID（`senderStaffId`）；单项不超过 256 字符；不能包含空行或 NUL | 用户执行白名单。不在白名单内的用户不能触发任务。 |
| `admin_users` | string array | 飞书必填；钉钉可先留空 | 问答模式 1–49 个；钉钉识别模式允许与白名单同时为空；必须全部同时出现在 `allowed_users` 中 | 管理员列表。 |
| `chat_name_prefix` | string | `Codex` | 非空 | 用于新会话群名标识。 |
| `auto_create_chat` | bool | 飞书新连接为 `true`，钉钉为 `false` | `true`, `false` | 飞书允许自动创建会话群聊或话题；钉钉不使用此字段。启用 `watch` 的目标连接必须开启。 |
| `conversation_mode` | string | `group` | `group`, `topic` | 飞书中 `group` 表示一个 Codex 会话一个独立群；`topic` 表示共享群内用话题隔离。只影响飞书新建或尚未绑定的会话；钉钉固定 `group`，内部按问答隔离，不创建群。 |
| `shared_chat_id` | string | 空 | 仅 `topic` 模式使用；飞书必须以 `oc_` 开头；不超过 256 字符 | 可指定机器人已加入的共享群。留空时自动创建专用话题群。 |
| `business_scope` | string | 空 | 最多 8000 字符 | 钉钉统一业务范围说明；所有成员与管理员同规则。空时不执行普通问答。 |
| `business_files` | string array | 空 | 最多 32 个绝对文件路径 | 钉钉显式提供的 UTF-8 业务资料；不允许符号链接或目录，每文件最多 256 KiB，总计 64000 字符。 |

共享群话题的安全边界：用户白名单只限制谁能触发执行，不隔离共享群的阅读权限。话题内容、历史入口和回复可能对该群所有成员可见；切换模式不会迁移、解散或重发旧独立群。

### 飞书凭证字段

凭证不在普通 JSON 配置中表达，必须在客户端输入并保存。

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `app_id` | 是 | 飞书自建应用 App ID。 |
| `app_secret` | 是 | 飞书 App Secret。留空输入表示保留已保存值；点击“清除本地值”并保存后会删除本地凭证。 |
| `verification_token` | 否 | 长连接模式通常不需要。 |
| `encrypt_key` | 否 | 按飞书后台事件加密配置填写；未配置则留空。 |

凭证主密钥保存在当前用户的系统钥匙串，密文保存在本机 SQLite。缺少钥匙串授权时不会降级为明文保存。

## 运行限制

| 字段 | 类型 | 默认值 | 允许范围 | 说明 |
| --- | --- | --- | --- | --- |
| `turn_timeout_seconds` | int | `900` | 30–7200 | 飞书创建会话资源的操作超时；钉钉用于本轮实际执行的时间预算，等待本机审批或补充信息时暂停计时。 |
| `approval_timeout_seconds` | int | `0` | `0`，兼容导入 15–1800 | 废弃的兼容字段，不再控制等待时间。授权、提问选择和表单不设等待有效期；网络请求仍有独立超时。 |
| `max_message_chars` | int | `2000` | 200–3000 | 单条 IM 输出消息的字符上限。 |
| `max_input_chars` | int | `24000` | 1–64000 | 发送给任务的输入字符上限。 |
| `queue_size` | int | `32` | 1–512 | 每会话调度容量；钉钉持久化待执行总容量为 `workers × queue_size`。 |
| `workers` | int | `4` | 1–16 | 并发工作者数量。 |
| `idle_push_ms` | int | `3000` | 1000–60000 | 进度合并间隔，单位毫秒。 |

## 会话自动发现

| 字段 | 类型 | 默认值 | 约束 | 说明 |
| --- | --- | --- | --- | --- |
| `enabled` | bool | `false` | `true`, `false` | 启用 Codex 会话自动发现。默认关闭，避免把已有本机会话意外分享到 IM。 |
| `connection_id` | string | 空 | 必须是已存在连接的 `id`；启用时必填 | 自动发现结果要同步到的 IM 连接。 |
| `interval_seconds` | int | `10` | 5–3600 | 扫描间隔。这是轮询，不是实时同步。 |
| `import_existing` | bool | `false` | `true`, `false` | 是否导入启动前已存在的 Codex 历史会话。默认关闭。 |

启用 `watch` 时，目标连接必须开启 `auto_create_chat`。未设置默认项目时可发现多个目录的 Codex 会话；请先确认这些会话适合分享到目标群成员。

## 完整导入示例

以下示例只包含非敏感结构字段，不包含飞书凭证。凭证仍需要在客户端中填写。

```json
{
  "work_dir": "",
  "state_dir": "",
  "codex": {
    "submission_mode": "native_queue",
    "auto_switch_thread": false,
    "preserve_desktop_draft": false,
    "auto_restore_desktop_draft": false,
    "mode": "follow_local",
    "binary": "",
    "home": "",
    "model": "",
    "sandbox": "workspace-write",
    "approval_policy": "untrusted",
    "cdp_port": 0
  },
  "connections": [
    {
      "id": "feishu-main",
      "type": "feishu",
      "allowed_users": ["ou_your_open_id"],
      "admin_users": ["ou_your_open_id"],
      "chat_name_prefix": "Codex",
      "auto_create_chat": true,
      "conversation_mode": "group",
      "shared_chat_id": ""
    }
  ],
  "limits": {
    "turn_timeout_seconds": 900,
    "approval_timeout_seconds": 0,
    "max_message_chars": 2000,
    "max_input_chars": 24000,
    "queue_size": 32,
    "workers": 4,
    "idle_push_ms": 3000
  },
  "watch": {
    "enabled": false,
    "connection_id": "feishu-main",
    "interval_seconds": 10,
    "import_existing": false
  }
}
```

## 常见配置错误

| 错误 | 处理方式 |
| --- | --- |
| 启动提示没有连接 | 至少添加并保存一个 IM 连接。 |
| 提示缺少 App ID / App Secret | 在对应连接中填写凭证并保存。 |
| 用户被忽略 | 确认发送者的 `ou_` Open ID 在 `allowed_users` 中。 |
| 管理员保存失败 | 将每个管理员同时加入 `allowed_users`。 |
| `watch` 无法启用 | 选择有效 `connection_id`，并开启该连接的 `auto_create_chat`。 |
| 共享群 ID 无效 | 确认是机器人已加入的 `oc_` 开头群 ID。 |
| 修改后仍使用旧配置 | 停止桥接后重新启动；后台进程不会在运行中热替换配置。 |
| 旧 JSON 导入失败 | 删除未知字段和明文凭证字段，在客户端重新输入凭证。 |


## 钉钉凭证、引用与本机执行

| 凭证字段 | 必填 | 说明 |
| --- | --- | --- |
| `app_id` | 是 | 企业内部应用的 Client ID / AppKey。 |
| `app_secret` | 是 | Client Secret / AppSecret，沿用客户端加密保存机制。 |
| `robot_code` | 是 | RobotCode，供应用机器人群消息发送接口使用。 |

钉钉连接使用 Stream，首版接收同企业群内的文本 @消息。白名单与管理员同时为空时为身份识别模式，只响应 `/whoami` 并返回消息发送者本人的 UserID；不进入问答队列，不调用模型，不自动写入授权名单。该固定回复沿用机器人发送权限，无需读取企业通讯录。已配置问答模式也保留此命令。白名单填写回调中的 `senderStaffId`；不使用飞书 Open ID，不接受跨企业身份。未配置自动建群、共享群 ID 或话题模式；桌面会话自动发现不能选择钉钉连接。

消息发送使用应用机器人的群发送接口，不依赖可能过期的 `sessionWebhook`。发送回执的 `processQueryKey` 与引用回调中的 `originalProcessQueryKey` 关联；平台返回可识别的原消息 ID 时也参与精确匹配。若所引用消息没有已记录的对应标识，拒绝续接并说明未执行，不按正文或最近会话猜测。

钉钉始终使用独立 CLI App Server，不受 `submission_mode` 的桌面选项控制。复用本机模型提供方、模型覆盖和认证；业务进程通过运行时覆盖关闭 Shell、网页搜索、Apps、插件、多代理、外部 MCP、hooks、外部通知、记忆及技能依赖安装。启动后核对有效配置和 MCP 状态，未明确禁用的能力拒绝启动；不会修改用户的 `config.toml`。已有 `sandbox` / `approval_policy` 桥接设置继续作用于飞书。

业务模型的 `environments` 必须明确为空，不开放宿主机环境；文档保存由程序完成，而不是给模型开放文件写入工具。业务问答只采用配置范围、所选资料和对话上下文；实时业务系统连接器不在本次开放范围。

CLI 创建问答时必须返回非持久化、无主机环境的会话（`ephemeral=true`、`path=null`、`environments=[]`），否则不提交问题。问答记录写入 IM Agent 的本机数据库；凭据继续由 Codex 从选定配置目录加载，不复制认证文件。每条问答的工作目录位于状态目录下的 `qa-workspaces/`，不自动选中代码项目。

后台重启或临时会话回收后，从最近已完成问答重建上下文，最多 30 轮、合计约 64000 字符，不恢复工具中间状态。单条历史回答过长时保留问题及回答末尾，并向模型标注截断。空闲会话定期回收以限制内存占用。提交或执行结果未知时，停止该问答续接；直接 @可另起问题，但不会撤销旧操作。

业务模式不向管理员放行主机命令、文件修改或扩大权限请求；所有人员统一受限。需求/缺陷补充问题直接在群内以正文提问，通过引用回复继续，不依赖本机弹窗。历史待处理交互仍在本机问答面板显示，但不能通过管理员身份绕过业务边界。

执行结果未确认时，当前问答停止续接，同时同步停止新任务调度并回收该后台生命周期的 CLI，避免未确认的旧轮次与新任务叠加。后台按已有运行意图恢复连接时，只恢复未提交队列；未知任务仍需核对。移除连接后，其未提交任务与待投递记录保留为失败状态，已提交的未知结果不变更为可自动重发。

发送状态中的平台成功回执表示钉钉已接受发送请求，不代表所有群成员收到或阅读；首版不查询异步发送状态。回执与引用关联使用同一条平台标识。缺失确定回执时标为未知，不立即换通道或重发。

问答记录保留用于续聊；现有飞书终态历史的 7 天清理不作用于钉钉问答。首版不包含钉钉图片、附件、交互卡片和自动建群。真实平台的引用回调及发送权限需在实际应用中联调，不能以静态检查替代。

发送者权限在排队执行前再次核对；修改人员名单并重启后，已失去授权的旧排队问题不会执行。


## 钉钉业务分类、文档与进度

业务范围为空时，保留固定身份识别与已授权人员的状态查询，不调用模型回答普通问题。非空时先做输入业务分类，分为普通问答、需求、业务 Bug、无关或需要说明业务关联；每轮追问都重新检查。管理员和普通人员没有内容范围差异。本次不增加敏感信息脱敏、按人频控或费用硬上限。

分类、业务回复和输出范围复核分别使用临时会话和结构化输出。需求/Bug只有 `clarifying` 或 `document` 能继续流程，不能以普通 `answer` 结束事项。真实 `grill-me` 与 `grilling` 通过 CLI 的技能列表定位，并作为显式技能输入附带到需求/Bug轮次；不可用时明确报错，不假称已加载技能。

文档正文保存在状态目录下的 `qa-documents/<question_id>/<event_id>.md`，文件权限为 0600，目录为 0700；使用目录句柄逐级拒绝符号链接，并采用不覆盖的原子发布。索引保存文档类别、轮次、标题、相对路径和 SHA256。读取时核对路径边界与摘要；本机界面仅在用户点击查看时加载正文。此文件路径属于存储说明，不是需要用户手工执行的操作。

接收、排队、范围检查、处理、生成、澄清等待、文档保存、发送、失败和未知结果分开显示。执行结果与通知/回答的投递结果也分开记录；错误反馈被平台接受，不代表原问题处理成功。需求澄清本轮回复已发送时，事项仍为澄清中，不标注文档已完成。

`/status`、`进度`、`进度查询` 解析引用的精确轮次；状态查询按入站消息身份去重，重复回调保留首次快照。通知与最终回答使用独立出站表。过期阶段及等待提醒在领取时取消；不可避免的网络在途重排通过采集时刻和“阶段记录”标识避免冒充当前状态。通知发送失败或未知不自动重发，不会阻塞最终回答的账本。

有效模型事件更新 `last_event_at`。长时间未完成时发送节流提醒，说明最后事件距今多久；不把定时器、Socket连接正常当作模型存活证据，不声称能据此确认提供方“挂了”或“正常”。身份命令和状态查询均不重新执行原问题。

本次 CLI 元数据兼容性核对使用 0.163.0；不据此承诺其它版本。未明确支持临时会话、无环境模式或有效配置验证的 CLI 会拒绝启动业务问答。
