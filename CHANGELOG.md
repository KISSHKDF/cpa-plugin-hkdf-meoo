### 2026.11.13.4

**品牌头像：插件列表里不再是一枚通用"插头"图标。**

### Added

1. **`logo.go`：PNG data URI 头像**（64x64，取自 meoo.com 首页 `<link rel="icon">` 声明的
   官方站点图标）。宿主的管理界面用 `<img src=...>` 画 `metadata.Logo`，此前该值是空串、界面
   回落到通用占位图标 —— 和 loomy 并排时认不出谁是谁。填 **data URI 而不是 https 外链**的
   理由：管理界面由本机 localhost 提供，外链会让一枚 56px 的装饰性图标依赖第三方 CDN，并在
   每次打开插件列表时外发一次请求。取舍、素材出处与哈希记在
   `docs/adr/ADR-0004-brand-logo.md`。
2. **`logo_test.go`：头像契约门禁**（同 loomy）。断言 data URI 前缀、base64 可解、PNG magic
   正确、IHDR 边长 ≥32 —— 空串或截断粘贴不会让本树其它任何判据变红，所以这条断言是必需的。

### 兼容性

无行为级变更：只改了注册结构的 `Logo` 字段并新增常量与测试。账号解析链、管理路由与面板
一字未动（本版不含对发现链的改动，那是 2026.11.13.3）。
### 2026.11.13.3

**修订：项目选择改为「带云实例的那个项目」——实机走查发现的缺陷。**

首个交付版本（2026.11.13.2）的选择规则是「projects 列表里第一个 `status=active` 的项目」。
实机上一个账号名下有 3 个 active 项目，只有第 3 个（`l1l7coxwlq0f`）带云实例、也只有它
能取到项目级 AK；插件选了第 1 个，随后云服务开通又被 Free 套餐的实例上限拒掉，于是面板
对**一个有可用项目级 AK 的账号**显示「还没有项目级 AK」——读数与事实相反。

### Fixed

1. **只读探测兜底**：`identity → projects（第一个 active）→ cloud → environment` 这条单发
   流程**一行未改**；只有当它**没能**拿到项目级 AK 时，才对**其余** active 项目追加一次
   **只读**探测（已存 url_id 优先，其余按列表顺序）：读 cloud state，只有 `enabled=true`
   才继续读 environment，拿到非空 `meoo_project_api_key` 即采用该项目。
2. **探测阶段结构性禁止写操作**：`probeProjectAK` 里不存在 `enableCloud` / `createProject`
   调用路径（不是靠开关守着）。阴性对照测试用假服务器断言「零 POST」钉住这一点：探测不得
   为候选项目开通云服务或新建项目——那会撞套餐实例上限，也会在账号里留下垃圾资源。
3. **note 诚实**：改选时追加「已改选带云服务的项目 `<url_id>`」并保留原有 notes；探测过但
   没找到时追加「已检查其余 N 个项目，均无可用项目级 AK」；单项目或首个 active 即命中时
   **不加任何 note、不发任何探测请求**（调用序列与 2026.11.13.2 逐字一致）。
4. **回归测试**：新增 4 个测试函数 / 8 个子用例——正例（3 项目取第 3 个）、阴性对照
   （全无云实例时零写操作）、不回归（单项目与首个即命中时调用序列逐字不变）、端到端
   （`refreshAll → assembleView` 面板行 `project_ak_state=ok`）。覆盖率 79.8% → 81.0%。
   变异自证：禁用探测 / 探测里插 `enableCloud` / 忽略 `cloud.Enabled` 三种变异全部判红。

### 2026.11.13.2

**首个交付版本：秒悟（Meoo）只读面板插件（`management_api` only，与 loomy 同形态）。**

秒悟的聊天通道继续由 `C:\CLIP\config.yaml` 的
`api-keys.openai-compatibility[name=秒悟]`（项目级 AK）提供；本插件不碰模型流量，
只把"这个 key 名下有多少积分、什么套餐、属于哪个项目、项目级 AK 是什么"读出来，
并且**只**读，不改上游任何东西（唯二的写操作是两个 provisioning POST，见下）。

### Added

1. **账号自管存储** `%USERPROFILE%\.meoo-cpa\accounts.json`（原子写：临时文件 +
   `os.Rename`；目录 0700 / 文件 0600）。首次启动创建空文件，**不预置任何账号**。
   两套凭据在同一个文件里：用户级 `meoo_ak_…`（走开放平台）与项目级
   `meoo_project_service_v1.…`（走模型网关）。
2. **发现链**（每账号一轮）：`identity → projects（第一个 status=active）→ cloud
   （未开通则开通）→ environment（取项目级 AK）→ points → entitlement`，全部
   打在 `https://meoo.com/open/v1`，`Authorization: Bearer <用户级 key>`。
3. **凭据族本地拦截**：把项目级 key 发给开放平台（或把用户级 key 发给模型网关）
   在**建请求之前**就被拒，并指明族别。否则上游回 401，看起来跟"账号失效"一模一样,
   面板会为一个粘贴错误宣布账号死亡。
4. **只读面板** `/v0/resource/plugins/meoo/panel`（裸 HTML，`Content-Type:
   text/html; charset=utf-8`，`Cache-Control: no-store`）：账号网格、积分/已耗、
   套餐、项目 url_id + 项目名、项目级 AK 尾号、key 尾号、状态胶囊、汇总卡。
5. **管理路由 8 条**（`/accounts`、`/refresh`、`/usage`、`/accounts/add`、
   `/accounts/remove`、`/models`、`/cache/reset`、`/health`）。`/accounts`、
   `/refresh`、`/usage`、`/accounts/add`、`/accounts/remove` 回**同一个载荷形状**；
   `/accounts/add` 失败时也回那个形状（外加 `"error"`），因为面板在错误时正好最需要
   看到原因。`/quota` 是宿主保留路径，本插件不使用。
6. **TTL 缓存 + 后台刷新 + 手动刷新**：`refresh_minutes` 默认 60（1..1440）。
   TTL 内读面板零上游调用；后台 tick 保持缓存热；`/refresh` 忽略窗口重走一轮。
   同一时刻只允许一轮扫描（single-flight），手动刷新与后台 tick 不会叠成 2×4 并发。
7. **三个配置键全部有真实消费链**（`dead-declaration` 门禁 GREEN）：
   `refresh_minutes`（TTL + tick 周期）、`auto_provision`（是否自动建项目/开云服务）、
   `project_name`（建项目时用的名字）。`enabled` 仍被解析与回显，但**不在声明面上**
   （声明面严格等于规格书 §6 的三键）。
8. **模型目录** `/models`：用第一个带项目级 AK 的账号读
   `https://api.meoo.host/meoo-ai/compatible-mode/v1/models`，与读数同一个 TTL。
   `qwen4-max-preview` 标记为 `usable:false` 并给出原因（网关固定 403 Access denied,
   阿里云限定 Token Plan，与账号无关）。

### 本机机队踩过的坑，本树逐条对治

| 坑 | 处置 |
|---|---|
| `pluginapi.ManagementRoute` 无 json tag，序列化出 PascalCase，宿主按小写解 → **每条路由静默 404** | `management.go` 用自定义镜像 struct + 小写 tag；`TestManagementRegistrationMirrorsUseLowercaseTags` 钉死 |
| 面板 HTML 套进 JSON 信封 → 浏览器显示信封源码而不是面板 | 面板走 `ManagementResponse.Body` 裸 HTML，HTTP 头 `text/html`；`TestPanelResourceServesRawHTML` 钉死（含 `/v8` 与 `/v0` 两扇门） |
| `host.http.do` 回包是 PascalCase（`pluginapi.HTTPResponse` 无 tag），按 `status_code` 解 → **静默拿到 0**，真 403 变成"站点没答" | 回包按 `{"StatusCode","Headers","Body"}` 解；状态 0 一律当失败（回落直连），绝不当成功；`TestHostHTTPResponseWireIsPascalCase` 钉死 |
| panic 穿过 cgo 帧会 abort **整个 CPA 进程** | 入口 `cliproxy_plugin_init` / `cliproxyPluginCall` 各有 recover；所有后台 goroutine 只经 `safeGo`（静态门禁钉死裸 `go func(` 计数 = 1）；panic 明细有界且非 string/error 只留类型 |
| ABI 长度闸门复制来的是 `1<<31-1` | 改成机队口径 `256 << 20`，并在 `C.GoBytes` **之前**判定；`abi_guard_test.go` 断言 `<= 256<<20 && < 1<<31` |
| 凭据回显进面板/API | 面板与 API **只**回 `id`（sha256 前 16 hex）、`key_tail`、`project_ak_tail`（各后 4 位）；`TestAccountViewNeverEchoesFullKey` 钉死；上游文本进 note/日志前先过 `redactSecrets` |
| 上游文本无界进日志 | `note()` 截断到 160 字节并先脱敏 |

### 安全与代价

- 上游调用**全部只读**，唯二例外：`POST /cli-compat/projects`（建项目）与
  `POST /cli-compat/projects/{url_id}/cloud`（开云服务），两者都受 `auto_provision`
  控制，且仅在账号确实没有 active 项目 / 云服务未开通时才发。
- 直连回退（仅无宿主时）带 `CheckRedirect` 跨源拒跳：这两个请求都在头里带凭据,
  Go 默认客户端会把头复制到重定向目标。
- `清空缓存` 只清读数与模型目录缓存，**不碰**账号文件（两个文件分开存，正是为了让
  这个按钮不可能毁掉凭据）。

### 兼容性

全新插件，无历史版本。宿主 ABI：`ABIVersion=1`、`SchemaVersion=6`。
只声明 `management_api`；`model_provider` / `auth_provider` / `executor` /
`quota_provider` / `usage_plugin` / `scheduler` 全为 false。
