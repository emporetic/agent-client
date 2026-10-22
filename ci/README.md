# CI 与发行包

单元测试与打包都在 GitHub Actions 里跑（`.github/workflows/agent-client.yml`）。想临时验证 Go 版本装得上、依赖拉得动，手动触发一次 workflow 即可：手动运行只跑测试，不发版。

## 测试作业的护栏

- **零依赖**：`go.mod` 现在没有任何第三方要求，GPL-3.0 的许可证兼容性因此是免费的。出现 `go.sum` 就说明有人加了新依赖，作业会停下来逼你去看它的许可证。
- **不留部署痕迹**：`ci/publicize.sh` 扫整棵树，发现自建 runner、容器 registry、包代理或个人机器标签这类字符串就让作业变红。发布前跑的是同一个脚本。
- 格式检查、`go vet`、`go test -race` 与编译照常。
- 作业里那些 `sh ci/xxx.sh` 跑的是 dash（Linux runner 的 `/bin/sh`）：要么把脚本写成 POSIX（别用 `pipefail`、数组这类 bash 专用语法），要么在步骤里写明 `bash ci/xxx.sh`。

## 客户端发行包

`ci/build-client.sh` 为 Linux/amd64 和 macOS/arm64 生成 `.tar.gz` 与 `latest.json`。每个包包含客户端二进制、完整 Agent skill、`examples/` 下的适配器及共享参数解析器、适配器配置说明；测试文件不随包分发。构建读取当前工作区，所以发布前应核对未提交内容并完成检查。产物默认落在仓库里的 `.dist/`（已 git-ignore）。

先把产物放进 staging 目录，校验后再发布：

```bash
VERSION=v0.3.2-heartbeat-resync DIST_DIR=../.tmp/client-release-v0.3.2-heartbeat-resync ./ci/build-client.sh
```

`latest.json` 记录版本号、包名、平台、SHA-256 与字节数。后端通过 `/api/v1/client/latest` 把同一份清单交给网页上手那一页；下载前先按清单核对 SHA-256 和大小，再解包。

## 发到 GitHub Release

打上版本 tag 就是发布信号。`release` 作业在测试通过后运行，把每个平台的包、`latest.json` 和一份写好的发行说明作为 GitHub Release 的附件放出去，并把这一版标成 latest。

版本号由 `ci/publish-github.sh` 打：它把工作分支压成一个干净 commit 推上 GitHub，再按 `PUBLISH_VERSION` 标一个 tag，tag 一落地就触发发布。tag 必须是 stable 的 `vMAJOR.MINOR.PATCH`（如 `PUBLISH_VERSION=v0.1.0`）——脚本先在这里拦下别的形状，`release` 作业也会拒绝；Release 不是 draft 也不是 prerelease 并标成 latest，后端与客户端才认它是 stable。本地自己打包爱用什么标签都行（`v0.3.2-heartbeat-resync` 这类一次性标记不进 GitHub），只有推上去的 tag 才算发布。

tag 指向的正是公开仓库那份树，所以发行包里有什么，取决于那次推送的内容。

发布之前在家里跑一遍发布作业那两道护栏就行（`go test` 的工作目录在被测包里，所以路径要给成绝对的）：

```bash
VERSION=v0.1.0 sh ci/build-client.sh
EMPORETIC_TEST_ARCHIVE="$PWD/.dist/v0.1.0/emporetic-agent_darwin-arm64.tar.gz" EMPORETIC_TEST_VERSION=v0.1.0 go test -run TestBuiltReleaseArchive ./internal/client
```

Actions 在运行机上取的是它自己那个平台的包（ubuntu 就是 `linux-amd64`），本机只能验到自己这份。

## 跨模块联调

客户端与真实后端的联调套件住在 backend 仓库（`tests/agent-client-backend.test.mjs`），由后端作业把本仓库克隆到 `peers/` 后编译 `emporetic-agent` 运行。本仓库的 CI 只跑单测。
