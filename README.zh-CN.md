# Plexus

[English](README.md)

Plexus 把你已经在用的编码代理接进 Slack，成为**伙伴**：**Claude Code**、**Codex**、**DSH**，以及任意 **ACP** 代理。

- 每个伙伴是一个独立的 Slack App，有自己的身份、连接、工作目录和原生会话。
- 你在 Slack 线程里交代目标。伙伴们自己讨论分工、互相交接；真正需要决定时在原处问你；最后交付可核验的成果。
- Windows 笔记本上一个 `plexus.exe` 拉起并守护所有伙伴。

Plexus 只是一层薄桥，不自己做 agent 循环。各 harness 保留自己的登录、模型、工具、MCP、skills、子代理和后台任务。Plexus 走它们的原生协议：

- Claude：stream-json 控制协议
- Codex：app-server
- DSH：bridge 插件
- 其他：ACP

> 状态：**v0.1 骨架**。已对 fake harness 和 fake Slack 做了单元测试和集成测试，**还没有在真实 Slack、真实 harness、真实 Windows 上跑过**。见[未验证](#未验证)。

## 快速开始（3 步）

1. **构建或下载** `plexus.exe`：
   ```
   CGO_ENABLED=0 GOOS=windows go build -trimpath -ldflags="-s -w" -o plexus.exe ./cmd/plexus
   ```
2. **运行 `plexus setup`**。
   - 它会探测已安装的 harness（不发任何模型请求），为每个探测到的 harness 添加一个伙伴。
   - 然后打开一次性的 `http://localhost:<端口>/s/<随机串>/` 页面。

   在页面上：
   1. 填你的 Slack 用户 ID（个人资料 → ⋯ → *复制成员 ID*）。陌生人边界保持开启。
   2. 每个伙伴点 **Create the Slack app**，Slack 会打开预填好的 manifest（Socket Mode、交互、权限范围）。依次完成：
      - 创建 App；
      - 在 *Basic Information → App-Level Tokens* 生成带 `connections:write` 的 `xapp-…`；
      - *Install to Workspace*；
      - 把 `xapp-…` 和 Bot User OAuth Token `xoxb-…` 粘贴进来，点 **Save**。

      也可以填 Client ID / Client Secret，走 **Install to workspace**（OAuth，回调到 localhost）。
   3. DSH 伙伴点 **Install the plugin**。插件打包进 exe 之后才可用，见 [docs/DSH_BRIDGE.md](docs/DSH_BRIDGE.md)。
3. **把伙伴邀请进频道**（`/invite @Plexus Claude …`），@ 其中一个交代目标，之后在线程里协作。
   - 可选：`plexus install-task` 注册登录时隐藏启动的计划任务。

密钥：

- Slack token 只存在 DPAPI 加密文件 `%APPDATA%\Plexus\secrets.dpapi` 里；`config.json` 不含任何密钥。
- Linux/macOS 仅供开发，需设置 `PLEXUS_INSECURE_DEV_SECRETS=1`，token 存成 0600 的明文文件。

## 命令

| 命令 | 作用 |
|---|---|
| `plexus run [--setup] [--hidden]` | 运行所有伙伴（默认命令）。所有伙伴完成首次连接尝试后，在 stderr 打印 `plexus ready bots=<n>` |
| `plexus setup` | 运行并打开设置页 |
| `plexus detect` | 以 JSON 输出探测结果：版本和登录**凭据迹象**，不输出值 |
| `plexus stop <task-id>` | 停掉一棵任务树：有 hub 在跑时经本机控制端口执行，没有时直接写库 |
| `plexus install-task [--exe PATH]` | 注册 Windows 登录计划任务（内嵌 `scripts/install-task.ps1`） |
| `plexus --version` | 输出版本；不读配置，不开库 |

`--config DIR` 指定配置目录；`PLEXUS_HOME` 同时覆盖配置目录和数据目录。

## 工作方式

- **一个线程就是一个任务。**
  - 你的根消息 `<channel>:<ts>` 就是任务 id。
  - 每个 (伙伴, 线程) 绑定一个原生会话，重启后也不失忆。
- **伙伴能看到所在线程的全部消息，要不要回应由它自己判断。**
  - 被人直接 @ 的轮次，最终文本照常发出。
  - 由伙伴消息、后台任务或交付触发的是自主轮次，只有调用 `plexus_post` 才会发帖。没事时安静。
- **轮次进行中来的新消息会并入当前轮。**
  - 方式：Claude 排队输入、Codex `turn/steer`、DSH `plexus.steer`。
  - 永远不回"我很忙"。
- **宿主工具**（以原生工具形式提供）：
  - `plexus_post`：在本线程发帖。
  - `plexus_delegate`：带六字段记录交接给其他伙伴。
  - `plexus_ack`：接手方确认节点，此后 `done_when` 锁定。
  - `plexus_deliver`：交付帖，附产物、SHA-256，并写 `out/MANIFEST.sha256`。带 `node` 时交付该节点并唤醒委派方。
  - `plexus_review`：委派方验收（accept，关闭节点）或打回（reopen，附原因并唤醒接手方）。同一原因打回两次，升级一次给 `owner_if_stuck`。
  - `plexus_stop_tree`：只在你触发的轮次里可用。
- **防空转。**
  - 伙伴之间反复说话、中间没有任何工具工作时，暂停投递这些闲聊；有人发话或出现新的工作就恢复。
  - 没有轮数上限，没有预算，没有超时。

### 交接记录

每次 `plexus_delegate` 都带下面六个字段：

| 字段 | 含义 |
|---|---|
| `task` | 一行任务描述 |
| `inputs` | 相对路径、permalink 或 URL |
| `done_when` | 可逐条检查的完成条件 |
| `evidence` | 预期或实际的证据 |
| `tried_failed` | 已知走不通的路 |
| `owner_if_stuck` | 卡住时找谁；默认是委派方 |

记录存在本地库里，并以卡片形式发在线程中。它的作用是清楚、可追溯，**不是鉴权**。

## 信任模型

- **你和伙伴之间完全信任。**
  - 没有权限分级，没有签名令牌，伙伴之间不审批。
  - 身份只看 Slack 的 `user` / `bot_id`，与 `owners` 和各伙伴的 Slack 用户 ID 比对。
- **陌生人边界是唯一的边界。** 默认开启，可在设置页关闭。
  - 陌生人指既不是你、也不是伙伴的人，包括其他 App 和 workflow。
  - 陌生人的消息只能换来对话：核心以 `chat` 级别下发这一轮，每个适配器都必须用 harness 的原生锁（**GuestLock**）执行它，不允许任何写入或执行。可以读工作目录内的文件，但不能联网抓取、委派、交付或叫停。各 harness 的锁定方式：
    - Claude：每个工具调用都会触发 PreToolUse hook，除工作目录内只读和 `plexus_post` 外一律拒绝。
    - DSH：prompt 上带 `level: "chat"` 和 `guest: true`，由 bridge 插件执行。
    - **Codex 和 ACP 没有硬性的原生锁**：只读沙箱下，"安全"命令不经询问就会执行。所以它们直接不理陌生人：边界开启时只回一句"I can only take requests from my team here."，不启动任何轮次。
  - 伙伴发的消息，继承它发出时所在轮次的信任来源。来源未知时按陌生人处理（fail closed）。
- **危险操作必须由你点击批准。** 这是唯一的审批关口。
  - 分类器**只有一个**，用 Go 写。所有适配器（包括 DSH）的每个工具调用都先经 `harness.Normalize` 补全类型、命令和路径，再交给它判断。
  - 它拦截以下操作：
    - 强推或改写历史：`git push --force/-f/--force-with-lease/+ref/--mirror/--delete`（`push` 前带 `git -C dir`、`git -c k=v` 等全局参数也算）、`filter-branch`/`filter-repo`/bfg、HEAD 已推送时的 `rebase`/`reset`/`commit --amend`；
    - 删除工作目录以外的文件；
    - 注册表、服务、计划任务、系统目录、机器级环境变量、安装程序；
    - 邮件、webhook，以及名字像发消息的 MCP 工具；
    - Plexus 读不到命令内容的 shell 调用（`exec.opaque`）。
  - **暂挂。** 只挂起这一个调用，不挂起整轮：
    - 权限回调立即拒绝这个调用，理由是 `已暂挂，等 Sin 批准`。伙伴可以继续做别的，或结束这一轮。
    - 线程里出现带 **Approve / Deny** 按钮的审批卡。
    - 你批准后，伙伴再次发起**完全相同**的调用时放行，**只放行一次**。Plexus 会告诉伙伴可以重试。
  - **只有你本人的点击算数。** 也可以在线程里回复 `approve` / `批准` / `deny` / `拒绝`。
  - 重启后，待批卡片原地更新，不会重新发帖。stop 会把待批卡片变为拒绝。
  - 在 `config.json` 里调整：
    ```json
    "dangerous_actions": {"use_defaults": true, "disable": ["git.rewrite"],
                          "extra_commands": ["^terraform apply"], "extra_mcp_tools": ["deploy"]}
    ```
  - 剩余缺口：Plexus 只能看到命令这一层，看不到脚本内部做什么（`./deploy.sh`、`make release`）。git alias 或 hook 也可能把普通 `git push` 变成强推。

## 启动伙伴前的检查

Plexus 启动每个伙伴前都会检查：

- **访客锁。** 陌生人边界开启时，harness 要么在原生层锁住陌生人的轮次（Claude、DSH），要么直接不理陌生人（Codex、ACP）。如果某个 harness 会接收陌生人消息、却没有原生访客锁，这个伙伴**拒绝启动**，日志里写明原因。
- **审批关口。** harness 必须有阻塞式的权限回调，否则危险操作关口守不住。如果你愿意自担风险，可以在 `bots[]` 里给这个伙伴设 `"ungated_ok": true`。
- **`ungated_ok` 只豁免审批关口，从不豁免访客锁。**

## 与桌面应用共用会话

你也可以在 Claude Desktop、Codex 应用或 DSH 里打开同一个会话。Plexus 绝不往别处正在使用的对话里写东西。

- **每次续接前**都会检查会话是否在别处活跃：
  - Claude：`~/.claude/sessions/*.json` 里有存活进程持有该会话，或者 Plexus 上次使用之后、最近 2 分钟内对话记录有改动。
  - Codex：app-server 报告存在另一个活跃写入者。
  - DSH：会话租约被占用。
- 如果在别处活跃，Plexus 只发一次："⏸️ I did not resume this conversation… Close it there (or let it go idle), then send your message again."
- **工作目录记录。** `<数据目录>/locks/workdir-<hash>.lock` 记录哪个伙伴在哪个目录工作。它只在日志里给出警告，从不独占：多个伙伴可以共用一个仓库。
- **空闲卸载。** 线程 5 分钟没有动静，harness 进程就退出（仍有后台任务时除外）。空闲的伙伴不保留 harness 进程。下一条消息按保存的会话 ID 续接。
- Plexus 从不使用桌面版的 DSH profile 或 `dsh.cmd`，而是用自己的 `plexus` profile 启动 DSH。
- `sessions`、attach、take、fork、release 等命令推迟到以后的版本。

## 凭据

- DPAPI 里**只存 Plexus 自己的 Slack token**（`xapp`、`xoxb`）。
- Slack OAuth 的 client secret 只在设置期间存在内存里，最多 10 分钟。
- Plexus 从不存储、注入或清理 harness 的凭据。各 harness 用自己的登录。
- Plexus 只从 harness 的环境变量里移除 `CLAUDECODE` 和 `CLAUDE_CODE_SIMPLE`。看起来像认证信息的环境变量值会登记到日志脱敏器，从不写进日志。

## 叫停

叫停有三种方式：

- 在任务线程里发一条内容恰好是 `stop` 或 `停` 的消息。
- 在 Slack 或终端里执行 `plexus stop <task-id>`。
- 直接用文字说。伙伴会调用 `plexus_stop_tree`，只在你触发的轮次里有效。

执行过程：

1. 撤销整棵任务树。
2. 对后台任务执行 `stop_task`，对受影响的会话发原生中断。
3. 等 3 秒后强杀进程树（Windows 上用 Job Object）。

结果：

- 待批的审批自动变为拒绝。
- 只发**一条**"已停止 / stopped"。
- 这棵树之后不再启动任何轮次。你在线程里再发话时，会作为新任务继续，沿用原来的原生会话。

## 重启

- 入站消息先落库再 ack。
- 每条出站帖子的 Slack metadata 里带 `request_id`。发送中途被打断的帖子，会先查线程历史再决定是否重发，不会重复发帖。
- 被打断的轮次在原生会话上恢复，并提示伙伴"先核对实际状态，已完成的不要重做"。

## 接入新 harness

Claude Code（`claude_code`）、Codex（`codex`）、DSH（`dsh`）是原生 adapter；内置的 ACP 条目有 `gemini_cli` 和 `dsh_acp`。其他 ACP 代理只需配置：

```json
"acp_harnesses": [
  {"name": "my_agent", "executables": ["my-agent"], "args": ["--acp"],
   "cred_files": [".my-agent/credentials.json"], "cred_env": ["MY_AGENT_API_KEY"],
   "exe_env": "PLEXUS_MY_AGENT_EXE"}
],
"bots": [
  {"name": "my_agent", "display_name": "My Agent", "harness": "my_agent",
   "enabled": true, "workdir": "C:\\Users\\you\\PlexusWork\\my_agent"}
]
```

ACP 伙伴没有宿主工具，也没有 GuestLock：只回应 @，它的文本由委派方验收。

## 文件位置

| 内容 | 位置（Windows） |
|---|---|
| 配置（不含密钥） | `%APPDATA%\Plexus\config.json` |
| 密钥（DPAPI） | `%APPDATA%\Plexus\secrets.dpapi` |
| 数据库（bbolt） | `%LOCALAPPDATA%\Plexus\plexus.db` |
| 日志（已脱敏） | `%LOCALAPPDATA%\Plexus\plexus.log` |
| 本机控制端点 | `%LOCALAPPDATA%\Plexus\control.json`：127.0.0.1 端口和每次运行的随机串，退出时删除 |
| 伙伴工作目录 | `%USERPROFILE%\PlexusWork\<伙伴>`（可配置） |

## 未验证

以下部分按各家公开文档实现，只对 fake 测试过：

- **平台：**
  - 真实 Windows：Job Object 强杀、DPAPI、计划任务、隐藏窗口。
  - 真实 Slack：Socket Mode、消息 metadata、交互按钮、OAuth 回调。
  - 真实 harness。
- **Claude Code：** PreToolUse hook、`mcp_message` SDK MCP、`stop_task`、排队输入方式的 steer，以及续接检查所依赖的 `~/.claude/sessions/*.json` 记录格式。
- **Codex：**
  - `dynamicTools`（experimental）、`turn/steer`、只读访问参数的格式、`untrusted` 审批策略、"另一个活跃写入者"的确切报错文字。
  - 提升权限的沙箱进程是否仍在 Job 内。
- **DSH：** bridge 插件另行编写，**本构建没有打包**。`internal/adapters/dsh/bridge/` 是留给它的空槽位，契约见 [docs/DSH_BRIDGE.md](docs/DSH_BRIDGE.md)。DSH 需要 Node ≥22.19。
- **Gemini CLI：** ACP 启动参数。
- **密钥：** DPAPI 绑定的是你的 Windows 用户，以你身份运行的任何进程都能解密，包括完全受信的伙伴。

## 文档

- [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md)：验收标准
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)：按实现修订的架构，含与原设计的差异
- [docs/DSH_BRIDGE.md](docs/DSH_BRIDGE.md)：DSH bridge 协议

## 许可

MIT，见 [LICENSE](LICENSE)。
