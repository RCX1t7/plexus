# Plexus 验收与验证手册（VERIFICATION）

本手册说明如何运行验收仿真、性能门禁与 CI 检查，并给出 Sin 在 Windows
真机上的最终核对清单。配套文档：`docs/REQUIREMENTS.md`、`docs/ARCHITECTURE.md`。

测试代码位于 `tests/`，与产品同属一个 Go module（`github.com/RCX1t7/plexus`），
不新增任何直接依赖（仅 stdlib）。

---

## 1. 测试套件总览

| 包 | 作用 | 是否命中真实产品代码 |
|---|---|---|
| `tests/gate` | 危险操作关口向量表（直接调用 `internal/danger.Classify`） | 是（真实） |
| `tests/deps` | 直接依赖预算（直接依赖恰好为 slack-go、bbolt、golang.org/x/sys） | 是（go.mod） |
| `tests/perf` | 体积门禁、`--version` 冷启动、bbolt 持久写/去重吞吐基准 | 是（真实二进制 + `internal/store`） |
| `tests/realsmoke` | 用假 Slack 启动**真实二进制**，断言 ready 行与 Socket 连接 | 是（真实二进制） |
| `tests/fakeslack` | 假 Slack（Socket Mode + Web API），slack-go 兼容 | 测试基建 |
| `tests/fakeharness` | 四种原生协议（claude/codex/dsh/acp）的脚本假 harness | 测试基建 |
| `tests/simkit` | 场景编排、进程/账本断言 | 测试基建 |
| `tests/refhub` | **参考桩 hub**，仅用于自检套件（证明断言非空洞），非产品 | 自检用 |
| `tests/acceptance` | 行为场景：对 refhub 自检（默认），或对真实 hub（设 `PLEXUS_BIN`） | 二者 |

---

## 2. 运行方式

默认（快速，CI 用）——跳过重型自检：

```bash
go test -p 1 ./tests/...
```

> `-p 1` 串行跑各包：conformance 自检会起子进程做 RPC 回合，若与其他包的
> `go build`/子进程并发抢 CPU，偶发超时（纯环境争用，非逻辑缺陷）。CI 的
> `go test -race` 步骤同样加了 `-p 1`。

重型 refhub 自检套件（验证套件本身，非产品；进程组/账本/审批全链路）：

```bash
PLEXUS_SELFCHECK=1 go test ./tests/acceptance/ -timeout 600s
```

针对**真实 hub** 跑行为仿真（需要先构建真实二进制）：

```bash
go build -o /tmp/plexus ./cmd/plexus
PLEXUS_BIN=/tmp/plexus PLEXUS_INSECURE_DEV_SECRETS=1 \
  go test ./tests/acceptance/ -run TestAcceptanceSim -v
```

> 说明：Linux 需要 `PLEXUS_INSECURE_DEV_SECRETS=1`（0600 明文 dev 密钥文件）；
> `PLEXUS_HOME` 指向临时目录。真实二进制的 Socket Mode 通过 `config.json` 的
> `slack_api_url` 指向假 Slack。

### 真实二进制 × 假 Slack 冒烟

`tests/realsmoke` 构建真实 `cmd/plexus`，写出真实 schema 的 `config.json`
（`owners` 数组、`bots[]`、`slack_api_url`）与 `secrets.dev.json`
（键 `bot/<name>/bot_token`、`bot/<name>/app_token`），启动 `plexus run`
并断言出现 `plexus ready bots=N` 且 N 个 App 建立 Socket 连接。

```bash
go test ./tests/realsmoke/ -v
```

---

## 3. 性能门禁（本机为 Linux 测量；Windows 需在 Sin 笔记本复核）

| 门禁 | 阈值 | 实测 | 命令 |
|---|---|---|---|
| exe 体积 windows/amd64（`-trimpath -ldflags='-s -w' CGO_ENABLED=0`） | ≤ 25 MiB | **11.62 MiB** | `go test ./tests/perf/ -run TestBinarySizeWindowsAMD64 -v` |
| exe 体积 windows/arm64 | ≤ 25 MiB | **10.70 MiB** | 见 CI `builds-and-size` |
| `plexus --version` 冷启动 | ≤ 50 ms | **best 2.07 ms / avg 2.69 ms** | `go test ./tests/perf/ -run TestVersionColdStart -v` |
| `plexus run` ready（假 Slack） | ≤ 500 ms | **110 ms** | `go test ./tests/realsmoke/ -v` |
| 内存事件路由（harness 事件 → worker 分发，2 个伙伴并行扇出，无磁盘） | ≥ 20,000 events/s | **5.4M–16.2M events/s**（5 次，中位约 9.5M） | `go test ./internal/slackbot/ -run '^$' -bench BenchmarkEventRouting -benchtime 1000000x` |
| 持久写（bbolt，每次一个 fsync 事务，单写者） | ≥ 2,000/s | **2,481–2,960 writes/s**（5 次中 4 次；1 次因共享盒子磁盘抖动降到 1,458/s） | `go test ./tests/perf/ -run '^$' -bench BenchmarkDurableDedupWrites -benchtime 3s` |
| 去重吞吐（50% 命中，持久） | 无门禁（仅参考） | 2,619–3,236 ev/s | `go test ./tests/perf/ -run '^$' -bench BenchmarkDedupThroughput -benchtime 3s` |
| 3 空闲伙伴 RSS / CPU | ≤ 30 MiB / ≤ 1% | 见下（待在 Windows 复核） | — |

> 注（架构师 2026-10-09 裁定）：20k events/s 门禁只针对**内存路由**；磁盘门禁单独为
> 持久写 ≥ 2,000/s；去重没有 20k 门禁。不在单 bot 路径上用 bbolt Batch，也不做 group commit。
> 测量环境：2026-10-09，Linux amd64 共享容器（overlayfs），Go 1.25.0。该盒子裸 fsync
> 约 3,000–5,500 次/s，而 bbolt 每次提交做两次 fsync（数据页 + meta），所以持久写的上限
> 约为裸 fsync 的一半，余量取决于磁盘；Windows NTFS 需在 Sin 笔记本复核。
> 基准只在发布时跑一次（CI `release-benchmarks`，手动触发），不在每次 push 跑。空闲 RSS/CPU 门禁
> 需在 Windows 真机用任务管理器核对（Linux 侧可用 `tests/acceptance` 的
> `TestPerfIdleResources`，设 `PLEXUS_BIN`）。

---

## 4. CI（`.github/workflows/plexus-acceptance.yml`）

作业与本地等价命令：

```bash
# 1) 格式
gofmt -l $(git ls-files '*.go')            # 期望空输出
# 2) vet
go vet ./...
# 3) 直接依赖预算
go test ./tests/deps/ -run TestDirectDependencyBudget -v
# 4) race 全量
PLEXUS_INSECURE_DEV_SECRETS=1 go test -race -p 1 ./...
# 5) 跨平台构建 + 25 MiB 硬门禁
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o plexus-amd64.exe ./cmd/plexus
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o plexus-arm64.exe ./cmd/plexus
# 6) 全新 clone 可构建（校验 .gitignore 未隐藏 internal/secrets）
# 7) gitleaks 全历史扫描（配置 .github/workflows/gitleaks.toml）
gitleaks detect --config .github/workflows/gitleaks.toml
# 8) Windows 测试（windows-latest，continue-on-error：v0.1 打 tag 前需要，不阻塞）
go vet ./... && go test -p 1 ./...
# 9) 发布基准（仅 workflow_dispatch）
go test ./internal/slackbot/ -run '^$' -bench BenchmarkEventRouting -benchtime 1000000x -count 3
go test ./tests/perf/ -run '^$' -bench 'BenchmarkDurableDedupWrites|BenchmarkDedupThroughput' -benchtime 3s -count 3
```

动作版本已固定（`actions/checkout@v4`、`actions/setup-go@v5`、
`gitleaks/gitleaks-action@v2`）。gitleaks 本地实测：工作树与全历史（21 commits）
均 **no leaks found**（既有单测 fixture 以路径白名单豁免，均为显式假 token）。

---

## 5. Windows 真机核对清单（Sin）

在装有各 harness CLI 的 Windows 笔记本上，用 PowerShell：

1. **放置可执行文件**：将 `plexus.exe` 放到如 `C:\Plexus\plexus.exe`。
2. **版本与体积**：
   ```powershell
   C:\Plexus\plexus.exe --version
   (Get-Item C:\Plexus\plexus.exe).Length/1MB   # 应 ≤ 25
   ```
3. **配置**：`%APPDATA%\Plexus\config.json`（键：`owners`、`bots`、
   `stranger_guard`、`acp_harnesses`、`setup_port`、`slack_api_url`、
   `dangerous_actions`）。每个 bot：`name`、`display_name`、`harness`
   （如 `claude_code`/`codex`/`dsh`）、`enabled`、`workdir`、可选
   `exe`/`args`/`persona`/`extra_deny`/`ungated_ok`/`app_id`。
4. **首次令牌**：`plexus.exe setup` 打开设置页，按页粘贴各 App 的 bot/app token
   （令牌存入 OS 凭据库，绝不写入 config.json）。
5. **启动与就绪**：
   ```powershell
   C:\Plexus\plexus.exe run     # 控制台应打印 plexus ready bots=<N>
   ```
6. **空闲资源**：任务管理器查看 3 个空闲伙伴总 RSS ≤ 30 MiB、CPU ≤ 1%。
7. **逐项演练危险关口**（每类应弹一张卡，三按钮 `仅此一次 / 本任务内批准 / 拒绝`）：
   - 建一个临时 git 仓库 + 本地 bare 远端，试 `git push --force`；
   - 对 `workdir` 外的临时目录试删除；
   - 试 `reg add HKCU\Software\PlexusTest ...`（测试键）；
   - 向测试频道试 `plexus_post` 外发。
   未点按钮前动作不执行；`本任务内批准` 覆盖同类同目标，换目标/类仍再问。
8. **停止**：在线程发 `stop` 或 `停`，或 `plexus.exe stop <task-id>`；应恰好一条
   `已停止`，整棵进程树清空，hub 仍在运行。
9. **ungated/bypass**：给某 bot 传 `--dangerously-skip-permissions` 应拒绝启动，
   除非该 bot 配 `ungated_ok: true`。

---

## 6. 已知风险与缺口（详见验收报告）

- **关口只看命令行**：脚本内部、git alias/hook/refspec、任意 HTTP、拼接混淆均可能绕过；
  MCP 工具仅按名字匹配；模型可把被拦动作改写成脚本再逃逸。
- **关口向量现状**（`tests/gate`，直调真实 `internal/danger.Classify`）：
  本套件最初记录 13 项骨架旁路；builder 已修复其中 8+ 项（`cd` 改相对路径、
  子 shell `(git push -f)`、npx 包装、`regedit /s`、`Add-AppxPackage`、
  `*Installer.exe` 后缀、写系统目录/重定向、`curl … chat.postMessage`→`out.api`、
  outside-workdir 删除经 `Deletes` 字段），这些已转成**严格断言**作为回归守卫。
  **仍未修**：`$(...)` 命令替换未展开（`exec.opaque` 未触发）——记为 builder bug。
- **ready 行当前打到 STDERR**，评审要求 STDOUT——记为 builder bug（见报告）。
- **真实 hub 行为仿真**目前以 refhub 为参照验证；真实二进制已通过冒烟（启动+Socket），
  完整行为仿真需进一步对齐 harness 帧格式/host 工具协议（见报告“集成点”）。
- Windows 的 RSS/CPU/启动需在 Sin 真机复核（本手册数值为 Linux 测量）。
