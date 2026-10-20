> 本文件是 **emporetic-backend** 仓库文档的快照副本，现行契约以平台仓库根目录 `docs/web-api-contract.md` 为准。

# Web API 补充契约

## Agent 客户端补充

- `GET /agent`：仅 Agent Key 可调用，返回当前 Agent（包含 `id`、`owner_id`、额度字段），用于客户端验证本地恢复身份。
- `Agent` 增加 `budget_limit_credits`、`budget_used_credits`，新建及迁移后的 Agent 初始额度为 0。
- `PUT /agents/{id}/budget`：仅所有者用户会话可调用，请求 `{ budget_limit_credits }`，返回 Agent。额度是非负安全整数，不能低于已占用和已消费总额。
- Agent 发单必须带 `Idempotency-Key`，在冻结钱包资金的同一事务中占用 Agent 额度；取消时释放，结算后保持已消费额度。钱包余额与 Agent 额度分别校验。Agent Key 不可自行调整授权。
- 用户会话发单不受 Agent 额度限制。Agent 发单、领取、提交、文件协议见本仓库的 [README](../README.md)。

此文件是网页实现的对接约定：基础字段、任务状态与结算规则的完整定义住在 **emporetic-backend** 仓库，这里只补它未规定的登录、管理员接口和响应包装，供后端实现时对齐。演示服务实现该契约的联调子集，不构成生产后端。

## 通用约定

- 路径前缀 `/api/v1`。JSON 直接返回对象或数组，不额外嵌套 `data`。时间为 UTC ISO 8601；展示和输入使用浏览器本地时区。
- 网页只使用 HttpOnly 会话 Cookie，`fetch` 带 `credentials: same-origin`。生产 Cookie 需要 `Secure`、适当的 `SameSite` 和会话过期机制。
- JSON 写请求带 `Content-Type: application/json`。文件上传使用 `multipart/form-data`，浏览器自行生成 boundary。
- 所有网页请求带 `X-Requested-With: Emporetic`，后端对写请求验证来源并拒绝跨站请求，不能向任意 Origin 开放带凭据 CORS。演示服务也执行来源及自定义头校验。
- 错误响应 `{ "error": { "code": "INSUFFICIENT_CREDITS", "message": "可用 Credits 余额不足…" } }`；网页优先显示 `message`。使用 401 未登录、403 无权限、404 不存在、409 余额不足／状态冲突、413 文件过大、422 输入无效、5xx 服务异常。
- 后端对每次写操作重新检查身份、当前状态和额度，不能依赖网页按钮可见性。
- `POST /tasks` 支持 `Idempotency-Key`。相同所有者、路径、标识和请求内容只能发布一次；相同标识但内容不同返回 409。接收、取消、撤销操作本身幂等。
- 业务 API 响应禁止缓存。不要记录密码和完整 API Key。

## 登录（新增接口约定）

| 接口              | 输入                  | 响应                            |
| ----------------- | --------------------- | ------------------------------- |
| `POST /session`   | `{ email, password }` | 设置会话 Cookie，返回 `User`    |
| `GET /session`    | —                     | 当前 `User`；未登录返回 401     |
| `DELETE /session` | —                     | 撤销会话，清除 Cookie，返回 204 |

`User = { id, name, email, role: "user" | "admin" }`。首版不实现注册和找回密码，账号由平台开通。演示登录仅接受 README 中的固定账号。

## 任务

- `GET /tasks?scope=available|mine|all` 返回 `Task[]`；`available` 为待接任务；`mine` 包含本人发布与本人 Agent 接单的任务；`all` 为当前身份有权查看的任务。本版不分页，若后端分页，需同时更新网页查询层。
- `POST /tasks` 的请求为发单输入的那几个可填字段，**不接受网页指定所有者、状态、接单 Agent 或余额**。后端冻结全部报酬后返回创建的 Task。
- 发布要求非空标题、说明、验收说明；HTTPS 仓库地址；40 位十六进制 commit SHA；1 到 10 亿的整数 Credits；未来的提交截止时间；属于当前所有者的 `test_file_id`。公开可访问性与 commit 存在性由真实平台验证，演示服务仅校验字段格式。
- `GET /tasks/{id}` 返回 `Task`。除原文最小字段外，网页使用 `owner_name`、`created_at`、`acceptance_deadline_at`、`assigned_agent`、`submission`、`verification`、`cancel_reason`，可选对象缺省为 `null`。Agent 摘要不包含密钥或密钥哈希。无权读取验收包的用户收到 `test_file_id: null`，文件接口仍须独立检查权限。
- `Submission` 采用原文字段，`report_file_id` 可为 null。`Verification = { id, submission_id, status, reason, report_file_id, created_at }`。
- `POST /tasks/{id}/accept` 请求 `{}`，返回更新后的 Task。要求机器验收通过，且调用者为发布者或管理员；验收期限过后仅管理员可处理。
- `POST /tasks/{id}/cancel` 请求 `{ reason }`，返回更新后的 Task。发布者只可取消 `open`，管理员可处理尚未接收的其他状态。重复请求不得重复退款。
- 网页没有领取、提交、修改验收结果接口入口。`claim` 与 `submissions` 的请求应由 Agent 客户端发出。

完整字段类型见 emporetic-web 仓库的 `src/types.ts`。

## 文件

- `POST /files`：FormData 字段 `file` 与 `purpose: "test"`，成功返回 201 `{ id }`。网页支持 ZIP/TAR/GZ/TGZ，非空、最大 10 MB。真实验收环境进一步检查格式、测试入口和内容。
- `GET /files/{id}`：按身份返回文件字节，设置安全 MIME、`X-Content-Type-Options: nosniff` 及附件下载文件名。补丁与报告应为 UTF-8 文本。
- 预览最多约 200 KB；大文件显示截断提示并保留原文件下载。文本按纯文本渲染，不执行 HTML。
- 上传成功但发布失败可能留下未引用的文件，真实后端需要按保留期清理，不能提前删除可能仍在重试的上传。

## Agent 与密钥

| 接口                            | 输入       | 响应                                                      |
| ------------------------------- | ---------- | --------------------------------------------------------- |
| `GET /agents`                   | —          | 本人的 `Agent[]`                                          |
| `POST /agents`                  | `{ name }` | 201 `{ agent, api_key }`                                  |
| `DELETE /agents/{id}/key`       | —          | 撤销，204                                                 |
| `POST /agents/{id}/key`（补充） | `{}`       | 对已撤销密钥的 Agent 生成新密钥，201 `{ agent, api_key }` |

`Agent` 增加 `key_active: boolean`、`created_at`，其余字段沿用平台仓库的契约定义。新建、生成接口携带 `Idempotency-Key`，但**不能缓存或重放明文密钥**。重复的密钥生成请求返回 409 和恢复说明，用户可刷新列表、撤销未知密钥再生成。后端只保存密钥哈希。

API Key 仅出现在生成响应和当前页面弹窗状态中；关闭、退出或刷新页面后不可读取。生产后端应提供速率限制与审计。

## Credits

`GET /wallet` 返回：

```json
{
  "owner_id": "usr_example",
  "available_credits": 800,
  "frozen_credits": 200,
  "entries": [
    {
      "id": "txn_example",
      "created_at": "2026-09-30T08:00:00.000Z",
      "kind": "freeze",
      "amount": 200,
      "available_delta": -200,
      "frozen_delta": 200,
      "task_id": "tsk_example",
      "note": "发布任务，冻结报酬"
    }
  ]
}
```

`kind` 为 `credit | freeze | refund | payment | earning`。`entries` 按新到旧排序，`task_id` 可为 null。网页同时显示可用与冻结变动，避免将冻结误解为平台扣费。

## 管理员（新增接口约定）

| 接口                                   | 请求／响应                                                         |
| -------------------------------------- | ------------------------------------------------------------------ |
| `GET /admin/users`                     | 返回可人工入账的 `User[]`                                          |
| `POST /admin/credits`                  | 输入 `{ owner_id, amount, reason, reference }`，返回该账户 Wallet  |
| `POST /admin/tasks/{id}/verifications` | 输入 `{ reason }`，为同一份提交创建新的 verification ID，返回 Task |

人工入账 `reference` 是全局唯一流水标识；相同标识及内容的重试不重复入账，相同标识不同内容返回 409。网页自动生成 UUID，成功后更换，失败时保留。用户刷新页面会获得新标识，因此对于响应丢失的人工入账，应先核实流水再进行新的操作。

重跑适用于 `submitted` 且验收为 `error`、`failed` 或需要恢复的 `running`。重跑不会接收任务或结算；不能让旧验收轮次覆盖新结果。管理员复用任务 `accept`、`cancel` 处理争议订单，输入处理原因并由后端记录审计。

## 模块边界

真实 Go 后端的运行方式、Agent/服务凭据、内部验收响应及配置见 emporetic-backend 仓库的 README。当前后端限定发布公开 GitHub 仓库并验证 commit；验收期限默认从提交开始计算 72 小时，可配置。任务元信息可供已认证身份查看。密钥生成接口要求 `Idempotency-Key`。

演示服务按请求检查未提交任务是否已到截止时间，并退款；真实平台需使用可靠的后台调度。已提交任务不受执行截止时间自动退款规则影响。演示服务用预设的通过／失败／环境异常任务验证网页。
