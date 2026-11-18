# Happy 实现借鉴说明

- 上游：slopus/happy
- 固定提交：85aa1baf5a786f0d4d0f6bb2b2883be47d51564b
- 许可证：MIT，完整许可见同目录 LICENSE。
- 来源文件：packages/happy-cli/src/codex/codexAppServerClient.ts。
- 本项目改写位置：internal/codex/protocol_identity.go。
- 改写内容：兼容轮次标识/状态字段，以及按请求的真实轮次标识过滤陈旧完成事件。TypeScript 改写为 Go，并在标识冲突时拒绝使用该事件。

本项目没有采用“会话 idle 即完成”和“final_answer 条目即完成”的快捷判定；仍要求确切轮次的终态。桌面轮次结果查询、飞书投递账本及日志恢复由本项目实现。
