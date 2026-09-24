# zpj 日常维护

这份文档只描述 `a1647517212/sub2api` 上长期分支 `zpj` 的真实流程。不要按上游 README 的发版方式，也不要再维护 `klno`。

## 仓库角色

| 名字 | 地址 | 做什么 |
| --- | --- | --- |
| 本 fork | `https://github.com/a1647517212/sub2api` | 只在这里开发、推送、打标签、发版 |
| 上游 | `https://github.com/Wei-Shaw/sub2api` | 只跟它的 **版本标签** `vX.Y.Z`，不跟它的 `main` 日常提交 |
| KlN-4096 | `https://github.com/KlN-4096/sub2api` | 历史来源。`klno` 已停更，不要 fetch 后 rebase/merge 进来 |

本机工作区里 remote 的名字容易看反：

- `mine` = `a1647517212/sub2api`（推这里）
- `upstream` = `Wei-Shaw/sub2api`
- `origin` = `KlN-4096/sub2api`（不要 push）

GitHub Actions 跑在本 fork 上时，它的 `origin` 就是 `a1647517212/sub2api`，和本机的 `origin` 不是同一个仓库。

分支：

- `zpj`：唯一开发分支。指纹收敛、猎手、这次去掉的 Turn-State 注入都在这里。
- `main`：默认分支，只给定时 workflow 当宿主。每天被重建成 `upstream/main` 再加上本 fork 的 `.github/workflows/sync-upstream.yml`。不要在 `main` 上开发，也不要指望上面的 `VERSION` 提交能留下来。
- `klno`：历史分支，停在对齐 KlN-4096 的那个点。不要 force-push，不要再 rebase。

## 当前代码里和指纹有关、同步时不能改丢的行为

跟上游标签 rebase 时，冲突按下面取舍。选错一侧，指纹收敛会被上游改回去。

- 回合 metadata 保持本 fork 的原地改写。不要换成上游的 `json.Marshal`，那会把字符串重新编成 `\uXXXX`。
- 上游新增的出站调用和本 fork 的 `scheduleCodexSideCalls` 都留。
- 判定 Codex 上游继续用 `TargetsChatGPTCodexUpstream()`。函数如果多了 `model` 参数，把上游的新参数接上，不要退回旧签名。
- 额度请求继续用 `CodexCanonicalUserAgent()`。不要采用上游的 `originator: Codex Desktop`，也不要给额度请求加 `sec-fetch-*`。
- `guardOpenAICodexTurnState*` 继续负责剥掉跨账号回带的 `x-codex-turn-state`，并且仍然排在任何出站 turn-state 处理之前。
- 自动接管（`openai_turn_state_auto`）和手填覆写（`openai_turn_state_override`）已经删除，不要在同步时加回来。292 猎手仍然探测、入池，但不会把票注入真实流量，也不再因为缺票把请求拦下来。

同步 workflow 在推送前会跑：

```bash
cd backend
go build ./...
go test -tags=unit ./internal/service -count=1 -run 'CodexFingerprintConvergence|CodexAccountIdentity|CodexFingerprint|CodexDeviceWireProfile|OAuthPassthrough|BuildOpenAIWSHeaders|SetupTokenCompat|IngressSession|CPR|OAuthOnlyGroupPredicate'
```

另外会对照 `openai/codex` 的 `e763730`，检查 `sync-upstream.yml` 里 `CODEX_IDENTITY_PATHS` 那些文件有没有变。变了就失败、不发版。确认指纹逻辑仍然对齐之后，再把 workflow 里的 `CODEX_PINNED_COMMIT` 改成新的短 SHA。

## 日常改代码

```bash
git fetch mine
git checkout zpj
git pull --ff-only mine zpj
```

改完在 `zpj` 上提交，只推本 fork：

```bash
git push mine zpj
```

不要 `git push origin`。那是 KlN-4096。不要推 `klno`。普通提交用 fast-forward，不要 force。

`zpj` 被自动同步 rebase 过之后，本地如果还停在旧历史上，先看 `git fetch mine && git log --oneline HEAD..mine/zpj`。确认本地没有要留的提交后，再用 `git reset --hard mine/zpj` 对齐。有本地提交就先 `git rebase mine/zpj`。

## 跟上上游新版本

自动任务是 `.github/workflows/sync-upstream.yml`，名字是 `Sync upstream into zpj`。

- 定时：每天 `03:17 UTC`（北京时间 11:17）。GitHub 只从默认分支 `main` 触发 schedule，所以 `main` 上必须有这份 workflow。
- 也可以在 Actions 里手动 `workflow_dispatch`。`upstream_tag` 留空表示最新的 `vX.Y.Z`；要指定就填完整标签，例如 `v0.2.9`。

它做的不是 merge：

1. 用 `git describe` 找到 `zpj` 当前基于的上游标签，剥掉末尾的 `-zpj.N` 或历史的 `-klno.N`。
2. 若最新上游标签和这个基线不同，执行 `git rebase --onto <新标签> <旧基线> zpj`。
3. 有冲突就 abort，workflow 失败，邮件通知，**什么都不推**。
4. 没冲突才 `go build` 和上面的定向测试。
5. 检查 codex-rs 身份文件是否相对 `CODEX_PINNED_COMMIT` 漂移。漂移则失败，不推、不发版。
6. `git push --force-with-lease origin zpj`（这里的 origin 是本 fork），打附注标签 `<上游标签>-zpj.N`（N 从 1 递增，已有就加一），再 `gh workflow run release.yml` 发版。`GITHUB_TOKEN` 推上去的 tag 不会自动触发 `release.yml`，所以必须用 workflow_dispatch。
7. 无论前面的漂移检查是否失败，都会把 `main` force-with-lease 重建成 `upstream/main` + 当前 `zpj` 上的 `sync-upstream.yml`。`release.yml` 写到 `main` 上的 `chore: sync VERSION` 会被这次重建丢掉，这是预期。

不要 `git merge upstream/main`，也不要 merge `KlN-4096` 的 `klno`。`main` 每天都在动，合进来会带上整段无关历史，而且和这条 rebase 链叠在一起。

### 冲突了要人工 rebase

workflow 失败后，在干净的 `zpj` 上自己做同一条命令：

```bash
git fetch upstream --tags
git fetch mine
git checkout zpj
git status   # 必须干净

base=$(git describe --tags --abbrev=0 --match 'v[0-9]*' zpj | sed -E 's/-(klno|zpj)\.[0-9]+$//')
echo "current base: $base"
git rebase --onto v0.2.9 "$base" zpj   # 把 v0.2.9 换成这次的上游标签
```

冲突按上一节的取舍解决，然后：

```bash
git rebase --continue
cd backend && go build ./...
go test -tags=unit ./internal/service -count=1 -run 'CodexFingerprintConvergence|CodexAccountIdentity|CodexFingerprint|CodexDeviceWireProfile|OAuthPassthrough|BuildOpenAIWSHeaders|SetupTokenCompat|IngressSession|CPR|OAuthOnlyGroupPredicate'
git push --force-with-lease mine zpj
```

只有这种「rebase 到新的上游标签」才允许对 `zpj` force-with-lease。force 之前确认 `mine/zpj` 上没有别人刚推的提交。

人工 rebase 成功后，如果 `git describe` 已经落到新的上游标签上，当天的定时任务会认为基线相同，**不会再自动打 `-zpj.N` 标签**。要发版就按下一节手动打。

## 发版

正常发版不需要手打标签。同步 workflow 成功后会打：

```text
v0.2.9-zpj.1
v0.2.9-zpj.2   # 同一个上游标签上再次发版时递增
```

`release.yml` 监听 `v*` tag push，但 Actions 用 `GITHUB_TOKEN` 推的 tag 不会再次触发 workflow。同步脚本因此改为：

```bash
gh workflow run release.yml --repo a1647517212/sub2api --ref "$tag" -f tag="$tag"
```

自己补发或 dry run 时同样走 workflow_dispatch，不要指望 `git push mine "$tag"` 就会出包。

- 正式包：`simple_release=false`，`dry_run=false`。
- 只想验证：`dry_run=true`。
- 只要 x86_64 GHCR 镜像：`simple_release=true`。

发版成功后，`release.yml` 会把 `backend/cmd/server/VERSION` 提交到默认分支 `main`。下一次同步把 `main` 重建成上游时，这个提交消失。版本号以 tag 和 GitHub Release 为准，不要到 `main` 上看 `VERSION`。

产物在 `https://github.com/a1647517212/sub2api/releases`。

## 其它定时任务

- CI（`.github/workflows/backend-ci.yml`）：`zpj` 的 push 会跑。`main` 被排除，tag push 也不再跑，避免和发版抢时间。
- Security Scan：每周一 `03:00 UTC`。schedule 只能从 `main` 触发，但 checkout 显式指向 `zpj`。以前指向 `klno`，那会扫到已停更的依赖。

## 不要做的事

- 不要 force-push `klno`，也不要把 `zpj` 合回 `klno`。
- 不要在 `main` 上改功能。推上去也会在下一次同步被上游 `main` 覆盖。
- 不要把 KlN-4096 的移动分支 `klno` 当成上游。上游只认 Wei-Shaw 的 `vX.Y.Z` 标签。
- 不要为了“少冲突”去接受上游对 Codex 出站身份、UA、turn metadata 编码的改写。
- 不要把 `openai_turn_state_auto` / `openai_turn_state_override` 的注入做回来。
