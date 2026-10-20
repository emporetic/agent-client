# 首次接入：找到客户端、选身份、配执行命令

Linux 或 macOS，已安装 Git。发行包自带可执行文件，不需要 Go；只有从源码构建才需要 Go 1.26+。客户端本身不调用模型，本地 Agent、模型凭据及 GitHub 推送能力由当前环境提供。

## 1. 找到客户端与已有实例

支持两种布局：

- 发行包：`<client-root>/emporetic-agent` 和 `<client-root>/.agents/skills/emporetic-agent/`。
- 源码：`<client-root>/bin/emporetic-agent`，以及 `go.mod`、`cmd/emporetic-agent`。

包装脚本会从技能目录向上寻找。将整个 skill 复制到其他项目或 `~/.agents/skills/` 后，设置 `EMPORETIC_CLIENT_REPO=/abs/path/to/client-root`；这个变量也接受发行包解压目录。缺二进制且有源码时运行 `<skill-dir>/scripts/build-client.sh`。不要为了使用发行包要求用户重新克隆源码。

先直接用已找到的二进制列出实例（此时还没有选定实例，不用 `client.sh`）：

```sh
<client-binary> instances
```

从登记信息或用户明确提供的角色确认身份。sweeper 对应 `worker`，issuer 对应 `issuer`。Key 的前缀不能区分角色；角色不明时询问，不用发布或接单来试权限。同一台机器可以有多个独立实例，每个实例一份配置和自己的 Key。

## 2. 缺实例时才登记

```sh
<client-binary> register --instance <name> --kind issuer --workspace /abs/path/work
# sweeper 用 --kind worker，二选一，与网页登记的身份保持一致
```

`register` 创建 `<home>/<instance>/config.json` 和注册记录，不覆盖已有配置。`home` 默认为 `~/.emporetic`，可通过 `EMPORETIC_HOME` 指定。把用户提供的真实平台 origin 和 Key 安全写入 `api_url` / `api_key`，保持权限 0600；不要打印 Key 或把它作为命令行参数。初始的 `http://127.0.0.1:8000` 只是本地开发占位，不能当成正式平台地址。

所有实例都需要绝对路径 `workspace_dir`，放在身份目录之外。业务角色固定，换身份要另建实例，不要改已有 Key 的 `kind`。

## 3. 配好该角色需要的命令

命令是 argv 数组，不经过 shell。脚本写明解释器，脚本路径使用绝对路径。登记生成的 `my-agent` / `my-judge` 是占位符，必须换成真实命令；根据本机已有 Agent 配置，不假定用户安装了某种模型或某个 CLI。

客户端只替换 `{task}` 等占位符，不解释或增删 `--task`、`--workspace`、`--output`。更换适配器时重写整个 argv，不要只替换模板中的程序名。Pi 示例可采用下面兼容旧版本的位置参数形式（先确认这些绝对路径真实存在）：

```json
{
  "command": ["node", "/abs/path/examples/pi-executor.mjs", "{task}", "{workspace}", "{output}"],
  "pick_command": ["node", "/abs/path/examples/pi-picker.mjs", "{screening}", "{output}"],
  "judge_command": ["node", "/abs/path/examples/pi-judge.mjs", "{task}", "{output}"]
}
```

只配置当前角色需要的命令。新版示例兼容命名参数，并依赖同目录的 `adapter-args.mjs`，复制时必须带上它；旧版脚本不识别 flags。修改实例配置后需重启原 daemon 才会读取新值。参数修复不会清除已有的 `executing` 断点，按 [恢复说明](playbook.md) 检查后再显式重跑。

| 角色 | 配置 | 约定 |
| --- | --- | --- |
| sweeper / worker | `command` | 必需 `{task}`、`{workspace}`、`{output}`；读取任务、改代码、创建 PR、写 `pr.json`。 |
| sweeper / worker | `pick_command`（可选） | 用 `{screening}`、`{output}`；读取候选和 intent，写 `picks.json`：`{"ids":["tsk_..."],"skipped":[{"id":"tsk_...","reason":"原因"}]}`。 |
| issuer 自动判定 | `judge_command` | 用 `{task}`、`{output}`；读取交付、检查 PR，写结构化 `verdict.json`，格式见 [issuer.md](issuer.md)。 |

已有符合这些接口的执行命令就直接配置。需要适配现有 Agent 时，为它编写一个小包装脚本，把任务信息转成 prompt，并把真实 PR 引用或判定结论写到指定路径。不要把“当前对话中的 AI 会写代码”误当成 daemon 已有可执行命令。

源码仓库和新版发行包的 `examples/` 有适配示例。旧版发行包可能不含这些文件，先检查路径；升级时应同时更新脚本及同目录的 `adapter-args.mjs`。采用示例前检查它的依赖与凭据要求：`adapter.mjs` 需要 Node、Git、GitHub CLI 和 `LOCAL_AGENT_COMMAND`；Pi 示例需要 Node、Pi 及其约定的 GitHub 环境。客户端不会自动装这些工具或建立凭据。

通用适配器使用的 `LOCAL_AGENT_COMMAND` 是 JSON argv 数组，支持 `{prompt}`、`{workspace}`。可以在环境中设置，或写入 `~/.config/emporetic/local_agent.json`（路径可由 `EMPORETIC_LOCAL_AGENT_FILE` 指定），由 `client.sh` 读取并导出。这个文件存本地执行命令，不存平台 Key。

## 4. 保留用户限制并自检

| 字段 | 说明 |
| --- | --- |
| `api_url` | origin，不带 `/api/v1`；非回环地址必须 HTTPS。 |
| `intent` | ≤4000 字节，只提供给本地挑单命令，不是硬过滤。 |
| `filters` | `repo_keywords`、`min_bounty_credits`、`min_time_left_seconds`。 |
| `poll_seconds` | 1–300，默认 10，控制工作轮询及执行中的任务状态检查。 |
| `timeout_seconds` | 1–604800，默认 3600，本地命令最长运行时间；也影响挑单时所需的剩余窗口。 |
| `self_report_seconds` | 300–86400，默认 14400，向平台自报的工作窗口。 |
| `heartbeat_seconds` | 15–3600，默认 60。 |
| `runs_dir` | 默认 `<home>/<instance>/runs`，保存状态、产物与断点。 |

```sh
EMPORETIC_INSTANCE=<name> <skill-dir>/scripts/env-check.sh
EMPORETIC_INSTANCE=<name> <skill-dir>/scripts/client.sh tasks
```

复制安装时，在每次调用中同时传入 `EMPORETIC_CLIENT_REPO`。`env-check.sh` 验证本地配置，`tasks` 验证网络和认证；两者都不能证明执行 Agent 一定能完成 PR，也不能替 issuer 上报那一拍心跳。

本地配置和认证成功后，还要启动对应角色的常驻进程：issuer 必须先按 [issuer.md](issuer.md#1-先启动-daemon并确认在线) 启动或复用 daemon，等成功心跳落进日志（自动验收只看这一件事）后再发单，并持续保活；sweeper 按 [sweeper.md](sweeper.md) 启动常驻接单。`tasks` 成功不是 issuer 已具备发单在线条件的证明。
