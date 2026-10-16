# IM Agent 配置参考

本文是字段级配置说明。推荐通过 IM Agent 客户端配置；客户端会把非敏感配置保存到本机 `desktop.sqlite`，把 App ID、App Secret 等凭证加密后保存到本机数据库，加密主密钥保存在当前用户钥匙串。

> 不要把 `config.json`、`desktop.sqlite`、钥匙串导出、日志或业务数据库提交到 Git，也不要发送给他人。`config.json.example` 只用于旧 JSON 导入预览，运行时不会从明文 JSON 或环境变量读取飞书凭证。

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
| `submission_mode` | string | `native_queue` | `native_queue`, `direct`, `desktop_queue` | 推荐 `native_queue`，即当前桌面会话输入。`direct` 表示 Codex App Server 直接提交。`desktop_queue` 是兼容值，不建议手工填写。 |
| `auto_switch_thread` | bool | `false` | `true`, `false` | 允许桌面输入模式切换到消息所属的已加载 Codex 会话。切换前会核对会话 ID；有未发送草稿时会等待或拒绝，不会覆盖草稿。 |
| `preserve_desktop_draft` | bool | `false` | `true`, `false` | 兼容模式下先加密备份可确认的纯文本草稿，再优先提交飞书任务。仅限桌面输入模式，重要草稿不建议依赖。 |
| `auto_restore_desktop_draft` | bool | `false` | `true`, `false` | 实验功能。仅在 `preserve_desktop_draft` 开启时才可能生效；默认必须保持关闭。 |
| `binary` | string | 空 | `custom` 模式可填 | Codex CLI 路径或可在 `PATH` 中找到的名称。留空时自动发现。`follow_local` 模式会清空该字段。 |
| `home` | string | 空 | `custom` 模式可填 | `CODEX_HOME` 目录。留空使用 `~/.codex`；必须是实际 Codex 配置目录。`follow_local` 模式会清空该字段。 |
| `model` | string | 空 | `custom` 模式可填 | 旧版兼容模型覆盖。推荐留空，让 Codex 使用本机配置。`follow_local` 模式会清空该字段。 |
| `sandbox` | string | `workspace-write` | `read-only`, `workspace-write` | 不允许无沙箱模式。 |
| `approval_policy` | string | `untrusted` | `untrusted`, `on-request` | 不支持更宽松的审批策略。 |
| `cdp_port` | int | `0` | `0` 或有效本机端口号 | 桌面 CDP 首选端口。`0` 表示自动探测；程序还会探测本机 9222–9235。端口必须只监听回环地址。 |

## IM 连接配置

当前首发渠道是 `feishu`。每个连接的 `id` 保存后不可改名，因为凭证和会话历史都按连接 ID 关联。

### 连接公共字段

| 字段 | 类型 | 默认值 | 约束 | 说明 |
| --- | --- | --- | --- | --- |
| `id` | string | UI 自动生成 `im-1` 等 | 1–64 字符；首字符为字母或数字；只能包含字母、数字、`_`、`-`；必须唯一 | 连接唯一标识。保存后不可修改。 |
| `type` | string | `feishu` | 当前支持 `feishu` | IM 渠道类型。保存后不可修改。 |
| `allowed_users` | string array | 必填 | 1–500 个；飞书必须是 `ou_` 开头的 Open ID；单项不超过 256 字符；不能包含空行或 NUL | 用户执行白名单。不在白名单内的用户不能触发任务。 |
| `admin_users` | string array | 必填 | 1–49 个；必须全部同时出现在 `allowed_users` 中 | 管理员列表。 |
| `chat_name_prefix` | string | `Codex` | 非空 | 用于新会话群名标识。 |
| `auto_create_chat` | bool | UI 新建连接为 `true` | `true`, `false` | 允许自动创建会话群聊或话题。启用 `watch` 的目标连接必须开启。 |
| `conversation_mode` | string | `group` | `group`, `topic` | `group` 表示一个 Codex 会话一个独立群；`topic` 表示共享群内用话题隔离。只影响新建或尚未绑定的会话。 |
| `shared_chat_id` | string | 空 | 仅 `topic` 模式使用；飞书必须以 `oc_` 开头；不超过 256 字符 | 可指定机器人已加入的共享群。留空时自动创建专用话题群。 |

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
| `turn_timeout_seconds` | int | `900` | 30–7200 | 单轮任务超时。 |
| `approval_timeout_seconds` | int | `300` | 15–1800 | 审批等待超时。 |
| `max_message_chars` | int | `2000` | 200–3000 | 单条 IM 输出消息的字符上限。 |
| `max_input_chars` | int | `24000` | 1–64000 | 发送给任务的输入字符上限。 |
| `queue_size` | int | `32` | 1–512 | 每会话等待容量。 |
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
    "approval_timeout_seconds": 300,
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
