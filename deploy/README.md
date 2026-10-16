# 后台部署入口

完整部署流程已迁移到 [`../docs/DEPLOYMENT.md`](../docs/DEPLOYMENT.md)。该文档覆盖源码构建、正式安装、首次配置、升级、回滚、CLI 运行、launchd 注意事项和 DMG 打包。

简版规则：

- 推荐使用桌面客户端概览中的“启动桥接”“停止桥接”和登录自启开关。
- 关闭客户端窗口不会停止后台；正式升级必须先停止桥接。
- “IM Agent 后台.app”只是启动入口，不是第二套配置系统。
- 不要同时运行桌面后台、前台 CLI、`--service` CLI 或旧 launchd 示例中的多个桥接进程。
- CLI 只读取桌面客户端 SQLite 配置，并使用同一用户的系统凭证库；不导入明文 JSON 凭证。
- `com.im-agent.bridge.plist.example` 仅保留给旧部署迁移检查；新部署优先使用 `io.im-agent.background`。
