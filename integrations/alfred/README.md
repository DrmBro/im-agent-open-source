# Alfred 独立启动入口

在 Alfred 中输入 `imagent`，选择“打开客户端”或“只启动后台”，按回车执行。输入 `imagent bg` 可筛选后台入口。

此工作流不查询 Spotlight，仅返回两个固定菜单项并打开已安装的应用路径：

- `/Applications/IM Agent.app`
- `/Applications/IM Agent 后台.app`

后台入口仍由原有启动锁与控制接口保证单实例，不重发旧问题、不修改登录自启。需要已有 Alfred 工作流功能。这里只增加独立入口，不修复系统 Spotlight 索引；普通 `IM Agent` 应用名称搜索仍取决于系统索引。

工作流源文件位于本目录的 `im-agent`。打包时将此目录内的 `info.plist`、`items.json` 和 `launch.sh` 放在 zip 根目录，并使用 `.alfredworkflow` 扩展名，通过 Alfred 导入。已安装机器使用独立工作流目录，不覆盖其他工作流。
