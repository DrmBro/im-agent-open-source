# IM Agent 部署与升级流程

本文面向在自己 macOS 设备上构建、部署和维护 IM Agent 的用户。部署前请确认你理解以下前提：IM Agent 不提供模型账号，不托管你的 Codex 或飞书应用；所有凭证、会话、数据库和任务都保存在本机。

首次部署优先按 [首次配置指南](GETTING_STARTED.md) 操作；让 AI 协助时使用 [AI 配置引导词](AI_SETUP.md)。本文补充构建、升级与维护细节。

## 1. 支持范围与依赖

| 项目 | 要求 |
| --- | --- |
| 系统 | macOS Apple Silicon；当前重点验证 macOS 15 附近版本 |
| Go | 1.24 或更高 |
| Wails CLI | v2.12.0 |
| Apple 工具链 | Xcode Command Line Tools |
| Codex | 用户自行安装、登录并验证可用 |
| 飞书 | 用户自己的自建应用，使用官方长连接 |
| Node.js | 前端没有 npm 构建脚本；开发工具依赖以固定版本的 `wails doctor` 检查为准 |

先检查基础环境：

```bash
sw_vers
uname -m
xcode-select -p
go version
wails version
wails doctor
```

如果 `wails` 未安装，使用固定版本：

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0
```

## 2. 构建安装包

建议从干净克隆或干净工作树构建，不要把本地数据、`.work/`、`dist/` 或调试数据库带进安装流程。

```bash
git clone https://github.com/DrmBro/im-agent-open-source.git
cd im-agent-open-source
go mod download
GOFLAGS=-trimpath wails build -clean
```

构建成功后核对产物和签名：

```bash
test -d build/bin/im-agent.app
codesign --verify --deep --strict build/bin/im-agent.app
```

## 3. 安装到正式目录

关闭旧的 IM Agent 客户端窗口。脚本不要求停止后台，但升级正式客户端前应先确认没有执行中的任务并停止桥接。

```bash
bash scripts/install-macos.sh build/bin/im-agent.app /Applications
```

脚本会：

1. 校验源 App 可执行文件与签名。
2. 生成 `/Applications/IM Agent 后台.app` 启动入口。
3. 把旧 App 移动到 `/Applications/.im-agent-backup.*`。
4. 注册 Launch Services，但不停止或重启现有后台。
5. 不自动开启登录自启。

安装完成后使用启动台或 `/Applications/IM Agent.app` 打开客户端。

## 4. 用户数据与凭证位置

macOS 默认数据目录：

```text
$HOME/Library/Application Support/IM Agent
```

主要文件如下：

| 路径 | 用途 |
| --- | --- |
| `desktop.sqlite` | 桌面配置、配置历史、加密凭证密文 |
| `runtime/` | 桥接业务数据库、会话映射、投递账本、状态卡和工作目录索引 |
| `bin/im-agent-service` | 客户端复制的稳定后台二进制 |
| `control.sock`、`control.token` | 仅本机同用户访问的控制接口 |
| macOS 钥匙串 | 保存用于解开本机凭证密文的主密钥 |

不要复制、发送或上传 `desktop.sqlite`、`runtime/`、日志、钥匙串导出或备份目录。升级前应在本机保留备份；迁移到新机器时需要同时恢复钥匙串访问，否则已有密文无法解密。

## 5. 首次配置

Codex 版本兼容、飞书具体权限/事件、网络与钥匙串授权的检查条件见 [首次配置指南](GETTING_STARTED.md)。以下是摘要，不替代首次收发验收。

### 5.1 Codex

打开“Codex 配置”：

1. 推荐选择“跟随本机 Codex”，客户端会优先发现桌面应用随附 CLI，再回退到 PATH 和常见安装路径。
2. 点击“重新检测本机配置”，确认已发现 CLI 和 `~/.codex/config.toml`。
3. 仍在 Codex 自身环境发送一个低风险问题，确认模型账号和代理可用。
4. 高级模式仅用于兼容特殊安装；自定义 `CODEX_HOME` 必须是实际 Codex 配置目录。
5. 沙箱只允许 `read-only` 或 `workspace-write`；审批策略只允许 `untrusted` 或 `on-request`。

推荐提交方式：

- `native_queue`：当前桌面会话输入。该模式会通过本机回环 CDP 发现 Codex 桌面入口，并在发送前核对会话与输入框状态。
- `direct`：兼容模式，直接使用 Codex App Server。
- 需要自动切换桌面会话时，明确理解消息可能会切换本机当前显示会话；有未发送草稿时会等待或拒绝，不会覆盖草稿。

### 5.2 飞书

在飞书开发者后台创建自建应用，启用长连接，并按实际使用能力申请最小权限。至少需要覆盖：

- 接收用户消息和交互卡片事件。
- 发送文本、富文本、交互卡片和附件。
- 创建、读取、更新或关闭会话/群/话题。
- 上传、下载或回传消息资源。

然后在 IM Agent 的“IM 连接”中：

1. 添加飞书连接，填写 App ID 和 App Secret。
2. Verification Token 与 Encrypt Key 按飞书后台配置填写；纯长连接场景通常可留空。
3. 填写 `ou_` 开头的飞书 Open ID 白名单；管理员必须在白名单内。
4. 选择会话模式：
   - `group`：每个 Codex 会话一个独立群。
   - `topic`：一个共享群内使用话题隔离多个会话。
5. 共享群模式下可填写机器人已加入的 `oc_` 群 ID；留空时按客户端提示自动创建专用话题群。
6. 保存配置。凭证不会回显；空白密钥输入表示保留原值。

字段级配置说明见 [CONFIGURATION.md](CONFIGURATION.md)。

### 5.3 运行限制与会话同步

默认限制适合个人使用：

- 单轮 900 秒、审批 300 秒。
- 队列 32、工作者 4。
- IM 单条展示 2000 字符，输入上限 24000 字符。

如需调整，请小步修改并重启桥接。`watch` 用于周期性同步已有会话；默认关闭。开启时必须选择已启用自动创建会话的连接，并设置合理间隔。

## 6. 启动与验收

1. 在概览点击“启动桥接”。
2. 确认后台状态为运行中。
3. 确认飞书连接状态为已连接。
4. 使用同时在白名单和管理员列表中的账号，私聊机器人发送 `/new`。
5. 填写表单首问并仅点击一次“创建并开始”；首问会自动尝试执行，进入新群等待回复，不要把首问再发一次。已有绑定会话可用 `/status` 查看状态。
6. 检查模型回复、状态卡、附件和投递回执符合预期。
7. 验证关闭客户端窗口后后台仍在运行；需要停止时回到概览点击“停止桥接”。

常用入口：

- `/new`：新建测试会话。
- `/status`：查看或刷新当前/最近请求状态，不会重发任务。
- `/cancel`：只取消本人尚未提交的附件组，不强行取消运行中任务。
- `/fork`：按项目实现分叉会话。

## 7. 升级流程

升级前检查：

- 没有执行中、等待投递或状态未确认的任务。
- 已在旧客户端停止桥接。
- 已关闭旧客户端窗口。
- 已备份本机数据目录；运行中的数据库不能作为最终备份。

推荐步骤：

```bash
backup="$HOME/Desktop/im-agent-backup-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$backup"
ditto "$HOME/Library/Application Support/IM Agent" "$backup/IM Agent"
```

构建新版并安装：

```bash
GOFLAGS=-trimpath wails build -clean
bash scripts/install-macos.sh build/bin/im-agent.app /Applications
```

安装后：

1. 打开新版 `/Applications/IM Agent.app`。
2. 确认配置和凭证状态正常。
3. 点击“更新后台程序（需先停止桥接）”。
4. 等待后台替换完成。
5. 启动桥接。
6. 核对实际后台状态、飞书连接和一次低风险任务。

仅替换 `/Applications/IM Agent.app` 不会热替换 `$HOME/Library/Application Support/IM Agent/bin/im-agent-service` 中的稳定后台；必须执行客户端内的后台更新动作。

## 8. 回滚

只有桥接已停止且客户端已退出时才回滚。

1. 停止新版桥接。
2. 退出新版客户端。
3. 找到最近一次 `/Applications/.im-agent-backup.*` 中的旧 App。
4. 恢复旧客户端和后台启动入口。
5. 打开旧客户端，必要时再次执行“更新后台程序”，让旧稳定后台回滚。
6. 如数据库结构已升级，不要直接用旧数据库覆盖新数据；先停止进程、备份新目录，并核对兼容性。

不能强停执行中的任务。状态为“已提交但未确认”的任务可能仍在执行，不应自动重发。

## 9. 登录自启与 launchd

推荐使用客户端概览中的登录自启开关。macOS 目标文件是：

```text
$HOME/Library/LaunchAgents/io.im-agent.background.plist
```

开关会在桥接停止后切换进程管理器，并保留配置和数据库。不要手工复制第二个 launchd 任务。

`deploy/com.im-agent.bridge.plist.example` 只用于检查旧部署。旧 `com.im-agent.bridge` 与新 `io.im-agent.background` 不应同时启用。

高级 CLI 运行方式：

```bash
go build -o im-agent-cli ./cmd/agent

# 前台调试：终端退出会停止
./im-agent-cli --data-dir "$HOME/Library/Application Support/IM Agent"

# 本机后台控制服务
./im-agent-cli --service --data-dir "$HOME/Library/Application Support/IM Agent"
```

CLI 只读取桌面客户端 SQLite 配置，并使用同一用户的系统凭证库。不要与桌面后台同时运行；数据库锁会阻止部分重复消费，但同一飞书应用被多个目录或多个进程消费仍会造成争抢。

## 10. 生成公开分发包

先在干净源码快照中构建：

```bash
GOFLAGS=-trimpath wails build -clean
```

汇总 Go 运行时与实际编入模块的许可证：

```bash
go mod download -json > go-mod-download.json
python3 scripts/collect-go-notices.py \
  build/bin/im-agent.app/Contents/MacOS/im-agent \
  go-mod-download.json \
  "$(go env GOROOT)/LICENSE" \
  THIRD_PARTY_NOTICES.txt
```

生成 DMG、SHA-256 与发布清单：

```bash
bash scripts/package-macos.sh \
  build/bin/im-agent.app \
  THIRD_PARTY_NOTICES.txt \
  dist \
  open-source-preview
```

脚本会校验：

- Apple Silicon arm64。
- 三段数字版本号。
- App 签名。
- App 内文件白名单。
- 不包含开发者主目录路径。

输出默认在 `dist/`，该目录被 Git 忽略。当前包为 ad-hoc 签名且未公证，不适合作为普通用户无提示下载的正式发布版本。正式开源发布应准备 Developer ID 签名、公证和明确的许可证。

## 11. 故障排查

| 现象 | 处理 |
| --- | --- |
| 提示钥匙串不可访问 | 解锁钥匙串并允许 IM Agent 访问；不要删除已有主密钥 |
| 缺少 App ID / App Secret | 打开“IM 连接”，补齐凭证并保存后重启桥接 |
| 未发现 Codex 桌面入口 | 确认兼容桌面端已启动，本机连接入口仅监听 `127.0.0.1`，端口范围为 9222–9235 |
| 后台状态未连接 | 关闭并重新打开客户端；不要重复启动 CLI 或 launchd 实例 |
| 业务数据库初始化失败 | 确认数据目录权限为当前用户私有，且没有第二个桥接进程持有锁 |
| 飞书消息收到但不回复 | 检查发送者是否在白名单、机器人权限、群/话题事件和网络代理 |
| 附件发送失败 | 检查飞书资源权限、文件大小、网络与任务状态；未知送达不要重复上传 |
| DMG 被 Gatekeeper 阻止 | 正式分发必须签名并公证；不要让用户关闭系统保护 |

## 12. 发布前安全清单

- [ ] 没有提交 `config.json`、`desktop.sqlite`、`runtime/`、日志、钥匙串、附件或 `.work/`。
- [ ] 文档、截图、录屏和 issue 没有真实 Open ID、群 ID、消息 ID、线程 ID或业务内容。
- [ ] 源码和示例中没有硬编码凭证、令牌、私有 URL、个人主目录或真实会话数据。
- [ ] Git 历史已单独审计；只要旧提交包含敏感数据，就不能直接把现有历史公开，应使用干净导出/孤儿分支重写发布历史并轮换泄露凭证。
- [ ] 桌面调试入口只绑定本机回环地址。
- [ ] 用户白名单非空，管理员在白名单内。
- [ ] Codex 沙箱和审批策略未放宽到不支持的值。
- [ ] 公共仓库已添加 LICENSE 和第三方许可说明。
- [ ] 发布包已记录版本、架构、签名状态、SHA-256 和是否公证。
- [ ] 升级后已核对实际运行版本、后台状态、飞书连接和一次低风险任务。
