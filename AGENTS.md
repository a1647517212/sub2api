# AGENTS

本仓库是 Wei-Shaw/sub2api 的 fork；长期工作分支是 `zpj`。

- 写码前读 `docs/zpj-maintenance.md`；协议事实以固定版本的 openai/codex 源码为准。
- 指纹行为改动必须运行该文档的单测和真实本地 HTTP/WS 接收端验收；真实上游结果单独记录。
- 不恢复 turn-state 自动接管、手填覆写和注入，不轮换已有指纹 seed。
- 未经用户本次任务明确授权，不合入其他分支变动；持久文档仅保留后续维护和验收必需的约定，临时证据不提交。
