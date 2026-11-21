## 2026.11.21.2

**每日登录赠送积分的三个补丁（2026-11-21，首版 2026.11.21.1 上线后的实机反馈）。**

### Fixed

1. **「需要人工登录」不能把当天锁死。**
   首版把 `needs_login` 也算成「今天已经尝试过」，于是 operator 就算后来在浏览器里登了、
   插件也不会再试一次 —— 当天的 2,000 分就白扔了，而且**没有任何提示**。
   现在 `needs_login` 不占用当天：后台轮与手动按钮都会继续再试，
   operator 登录完就能被领到（`TestNeedsLoginIsRetriedOnTheSameDay` 钉住）。

2. **operator 看到的是英文内部错误串。**
   `no session profile for this account: 本机没有属于该账号的已登录浏览器会话…`
   这种句子出现在面板和日志里，对要动手处理的人毫无帮助。
   现改为一句人话：**「请在浏览器里登录 meoo.com（阿里云账号），插件会自动复用那个会话；
   本插件不保存密码、不填短信。」**

3. **读浏览器 cookie 库时的锁。**
   Chromium 开着的时候会用独占方式持有自己的 cookie 文件，普通读法直接失败
   （`being used by another process`）。现改为**用共享读方式打开**（`FILE_SHARE_READ|WRITE|DELETE`，
   只读，绝不写浏览器 profile），再把内容复制到临时文件解析。
   （Windows 上浏览器进程仍可能持锁不放，此时该 profile 读不到 —— 这是平台限制，
   不是插件能绕过的；退路见下条「粘会话」。）

### Added

4. **「粘会话」退路：浏览器读不到时可以手工粘一段 Cookie。**
   新增两条管理路由：`POST /plugins/meoo/daily-login/session`（保存，account_id + cookie）
   与 `DELETE /plugins/meoo/daily-login/session`（清除）。
   **保存前必须核对身份**：这段会话要能被秒悟接受，且它的 `open_id` 必须等于该账号的
   `open_id` —— 不是这个账号的会话一律拒绝保存（`TestAPastedSessionIsAcceptedOnlyForItsOwnAccount`）。
   Cookie 与用户级 key 同等对待：只存本机账号文件，**永不在任何响应里回显**
   （`TestSessionCookieIsStoredAndNeverEchoed`）。面板每行多一个「粘会话」按钮。
   重新粘贴同一个 key **不会**丢掉已存的会话（`TestReaddingTheSameKeyKeepsTheStoredSession`）。

5. **会话来源多一处：operator 平常用的浏览器。**
   首版只扫 `%USERPROFILE%\.meoo-cpa\edge-profile\<账号id>`（本插件自己那套）。
   现同时扫描 Edge / Chrome / Brave 的日常 profile（**只读**），按身份认领。
   实测：账号「好看的风」正是在日常 Edge 里登的，首版看不到它。

### Verification

- 单元与回归测试全绿；新增 2 项变异测试确认用例真能拦住回归：
  把 `needs_login` 改回「当天锁死」、去掉会话存盘的保密纪律。
- 实机 live 测试仍全绿（读真 Edge profile、身份自动匹配、今天已领过的账号如实报 `already`）。

## 2026.11.21.1

**新增「每日登录赠送积分」自动领取：每天自动领秒悟的 2,000 分（2026-11-21）。**

### Added

1. **`daily_login.go` — 领取 2,000 分的机制本身。**
   秒悟的规则是「每日首次登录自动赠送 2,000 积分」，**没有领取按钮，也没有领取端点**：
   官网 0.0.113 的 3.2 MB bundle 全量扫描无任何 claim/signin/checkin 端点，积分流水是
   `type=LOGIN_GRANT / operator=SYSTEM / sourceType=DAILY_LOGIN / amount=2000`。
   唯一的发放触发点是 `GET https://meoo.com/api/mc/entitlement/init` —— 官网 SPA 启动时自己调它
   （失败按 500/1000/2000/4000 ms 退避重查），另在 `visibilitychange`→visible 时补调。
   本插件调的就是同一个端点。

2. **判据只看账号自己的账本，响应字段不作数。**
   每次尝试都先读一次账本作基线，调 `init` 之后再读一次：**账本上出现新的一笔
   「登录赠送积分」才算领到**。服务端答 `dailyLoginGranted=true` 而账本没动 ⇒ 判「未领到」；
   账本动了而响应说不清楚 ⇒ 判「已领到」。账本是权威，响应只用来解释。
   （2026-11-16 实测：2,000 的发放会当场被一笔 -472 的 `POINT_REPAY` 抵扣，余额只涨 1,528 ——
   所以判据必须落在**批次**上，不能落在余额上。）

3. **凭据是网页会话 Cookie，不是开放平台 AK。**
   AK（`meoo_ak_…`，Bearer）只能读 `/open/v1/cli-compat/account/points`，**调它不算登录、不发放**
   （实测：一个只被 AK 天天读、从未真正打开过站点的账号，账本里一笔赠送都没有）。
   所以这个功能需要**网页会话 Cookie**。本插件从本机浏览器 profile 里取（`session_cookie.go`）：
   Chromium 经典 v10 加密（`Local State` → DPAPI 解出的 32 字节 AES key → AES-256-GCM，
   明文前 32 字节是 sha256(host_key) 绑定前缀要剥掉），读取器与解密沿用灵犀树已在本机验证过的实现
   （`sqlite_min.go` / `chromium_cookie.go` / `dpapi_windows.go` 原样移植，零新依赖、零 cgo）。
   **不存密码、不填短信、不自动登录** —— operator 自己在浏览器里登一次，插件复用那个会话；
   会话过期时如实报「需要人工登录」，绝不谎报已领。

4. **profile 与账号的对应靠身份自动认领，不需要人工维护映射表。**
   开放平台的 `account.userId` 与网页会话的 `open_id` 是**同一个数**（实测：账号 1969 两边都是
   `1241690576128755`），插件据此把某个浏览器 profile 判给某个账号。

5. **配置项 `daily_login`（默认开）。** 关掉它插件就不再读任何浏览器 profile。
   管理面新增两条路由：`POST /plugins/meoo/daily-login`（立即领取，可带 `account_id` 只领一个）
   与 `GET /plugins/meoo/daily-login`（看每个账号今天的状态，零上游调用）。
   面板每行多一枚状态胶囊（今日 +2,000 / 今日已领 / 待登录 / 今日未领 / 未尝试），
   动作条多一个「领每日积分」，每行多一个「领每日积分」。

6. **后台自动轮。** 零点翻日后不久（本地日历日，不是 UTC —— 见下）自动尝试一次；机器夜里睡着
   就在开机后补跑。已领过的账号当天不再打扰上游（`init` 端点**零请求**）。

### Fixed

7. **业务日必须是本地日历日，不能用 `toISOString()`（UTC）。**
   这是会让整个功能**静默失效**的坑：本地 00:05 时 UTC 还停在前一天，按 UTC 算的「今天」会
   看到「今天的赠送已经到账」→ 整天跳过 → 一次都领不到。
   已在 `businessDay()` 钉死并有回归用例（`TestBusinessDayUsesTheLocalCalendarDayNotUTC`），
   变异测试确认该用例真能拦住回归。

8. **身份核对读不到 open id 时必须「拒绝」，不能「放行」。**
   第一版把开放平台的 `account.userId` 当 JSON 字符串读，而上游发的是**数字**
   （如 `1978784341143815`，还超出 float64 的精确表示范围）⇒ id 恒为空 ⇒ 身份核对退化成
   「这个 profile 是谁就是谁」⇒ **一个账号的会话可能被拿去领另一个账号的奖励**。
   现改为 `json.Decoder.UseNumber()` 取文本，且「id 读不到 ⇒ 拒绝该会话」，并补了
   两条回归用例（读不到就拒、属于别人就拒）与变异测试。

### Verification

- 单元与回归测试全绿；**4 项变异测试**逐一确认新用例真能拦住回归：业务日改回 UTC、
  信任服务端响应而不看账本、去掉「已领过就不请求」的短路、恢复身份核对的放行漏洞。
- **实机 live 测试**（`MEOO_LIVE_TEST=1`）读本机真 Edge profile：成功解出会话 Cookie 且**站点认**
  （`/api/v1/user/me` 返回真实身份），2 个 profile 与账号自动匹配成功；
  对今天已领过的 3 个账号，领取如实报 `already` 而没有谎报。
- 阴性对照：已领过的账号对 `init` 端点**零请求**；服务端说发了而账本没动 ⇒ 判「未领到」。

## 2026.11.20.2

**新增树卫生守卫：构建产物绝不许留在源码树里（2026-11-20）。**

- 新增 `tree_hygiene_test.go`（`TestTreeHasNoBuildArtifacts`）：扫全树（`build-out/` 与 `.git` 除外）的 `.dll/.h/.exe/.lib/.o/.a`，命中即在 `go test` 阶段报红并打印可执行修复指引（自检编译改用 `go vet`+`go test`；或 `go build -o` 指向树外；正式构件走 `scripts/build-ng.ps1`）。判据与发布门禁 `tree-no-artifacts` 同口径，且同样不读 `.gitignore`。
- 理由：自检时跑 `go build ./...` 会在含 `package main` 的目录里把可执行文件写到当前目录（`meoo.exe`），产物虽被 `.gitignore` 忽略但门禁扫真实文件系统 ⇒ 每次发版必被拦一次。同树历史已犯过多次，且前几次的记录只写进 CHANGELOG（没人会在 build 前翻它）—— 改成测试守卫才管得住。
- 阴性对照：放一个 `meoo.exe` 进树 → 断言当场报红并打印修复指引；删除后复跑绿。

## 2026.11.20.1

**截断改为码点安全：账号备注、身份显示名与项目名不再被切成半个汉字（truncate 码点自护）。**

### Fixed

1. **`main.go` 的 `truncate()` 原来是 `s[:n]`——Go 里按字节切。** 这棵树经它限长的全是
   operator 面对的中文文本：账号 `label`（即人写的备注，80 字节，`accounts.go:272`）、
   身份显示名（60）、上游文案 `note()`（160，进卡片 note 并落 `accounts.json` / 凭据文件）、
   management 路由错误（240）、`panicDetail`（240，进错误信封与日志）。一个汉字占 3 字节、
   emoji 占 4 字节，字节下标经常落在字符中间；切出的孤立尾字节经 `encoding/json` 变成 `U+FFFD`，
   在卡片上显示成乱码方块，并且每次重写同一条 note 都把它继续带下去。
   现改为回退到前一个码点边界（新 helper `truncateUTF8Bytes`），**字节上限一个也不变**，
   结果永远是合法 UTF-8；且 limit ≥ `utf8.UTFMax`（4）时保证至少留下一个完整字符。
   `truncate()` 的签名与语义（去空白 → 超限才截 → 追加 `…`）不变，全部调用点零改动；
   ASCII 文本的截断结果**逐字节不变**（有回归用例钉住）。
2. **`config.go` 的 `clampProjectName` 同样是 `trimmed[:64]`（字节）。** 项目名是 operator 手输
   文本、经常是中文，这个名字会被 POST 给上游 create-project 接口并持久化在上游——切碎后等于
   把一个非法 UTF-8 的名字发给站点，回读成了替换字符。现改为 `truncateUTF8Bytes(trimmed, 64)`：
   **64 字节上限不变**（仍严格不超过旧上限，不会因为修复而发出更长的名字），只是切在码点边界。

### Judged but not changed

- `upstream.go:664` `strings.ToUpper(part[:1])`（模型显示名首字母大写）：切的是 1 字节，不是限长；
  该值来自网关的机器 id（`qwen3.8-flash` 这类 ASCII slug），判定为不含中文，保留原样。
- `accounts.go` 的 `keyTail`（`key[len-4:]`，凭据尾部）与 `accountID`（`sha256[:16]` hex）：
  纯 ASCII 标识符，按字节切与按码点切等价，保留原样。

### Added

- `truncate_test.go`：**8 个**回归用例——①中文边界不碎（`utf8.ValidString` + 无 `U+FFFD` + JSON 往返）；
  ②emoji（4 字节）边界不碎；③纯 ASCII 与旧 `s[:n]` 逐字节一致；④边界组合例（恰在码点边界 / limit ≤ 0 /
  原长短于上限原样返回 / limit 小于一个字符时返回完整字符或空串）；⑤`truncate` 包装层端到端；
  ⑥`note()` 端到端（进卡片 note 的那条路径）；⑦`panicDetail` 端到端；⑧`clampProjectName` 端到端，
  并额外断言"200 字节 ASCII 名字仍恰好截到 64 字节"（证明上限没被顺手改大或改小）。
  **阴性对照已做**：把 helper 的 `return s[:cut]` 改回 `return s[:limit]`，8 例中 **7 例稳定变红**
  （报 "not valid UTF-8"）；唯一保持绿的是第③条——那是刻意钉住"ASCII 不许回归"的对照。
  改回即全绿。

## 2026.11.16.1

**积分条改画「手里还剩多少」：卡片与汇总条都从右边开始掉，与 loomy 面板同向。**

### Fixed

1. **卡片进度条方向反了**（用户 2026-11-16 反馈「每个具体账号的积分横条是从左向右掉积分的」）。
   旧模板先画 `cd-seg used`（已耗，灰）再画 `cd-seg`（剩余，绿），于是绿条随消耗**从左往右**
   长，读起来就像积分从左边被吃掉；loomy 的顺序相反：绿色「剩下的」贴左，灰色「用掉的」挂在
   右端，条子从右端缩短。现按 loomy 落地，并同 loomy 加 3px 段宽下限（`min-width:3px`，原来
   写 0，小额剩余会渲染成空条）；零宽段不发节点（有 3px 下限时，发一个 0 宽段会在轨道右端
   画出一条假灰边，看起来像"已经用掉一点"）。
2. **汇总条改成同一口径**：原来画的是「总已耗 / 总总量」的已耗进度，颜色按已耗占比取
   （`barColor(100-pct)`）。同一张卡上两条条子方向相反，是这次「积分到底掉没掉」误读的一半来源。
   现在画「总可用 / 总总量」，颜色按可用占比取，与卡片条一致。
3. **卡片百分比文案改成「手里还剩 X%」**（loomy 的写法），避免「已耗 51%」与「绿条 49%」
   看起来互相矛盾。

### Added

- `panel_layout_test.go: TestCreditBarsDrainFromTheRight`：位置断言（「剩下」段必须在「已耗」段
  之前）+ 零宽段过滤 + 汇总条口径 + `.cd-seg` 3px 下限。已做阴性对照：把两段顺序换回去，该
  测试即 FAIL。两段同属一个 class 家族，字符串存在性断言抓不到交换，只能断言位置。

### 兼容性

纯面板渲染变更：不动后端字段、不发上游请求、不改缓存格式。数字仍全部来自上游
`/cli-compat/account/points`（availableBalance / totalGranted / totalConsumed），取数逻辑未改。

## 2026.11.15.2

**SDK 快照抬钉：CLIProxyAPI v8 零件包 v8.0.16 → v8.0.72（2026-11-15 机队统一行动）。**

### Changed / Why

1. **抬的是什么**：go.mod 里的零件包快照从 2026-10-07 的 v8.0.16 抬到 v8.0.72。插件编译时把 SDK 静态打进 DLL，宿主跑哪个版本与它无关；抬钉只改变"插件上报的协议档位"（SchemaVersion 6 → 10）与可用能力面，不改变宿主对接行为。
2. **为什么现在抬**：40 天机队停在 v8.0.16（教训 L-903：构建断言把"当时的正确值"写成了永久约束）。抬钉后协议报告面与在跑宿主（同版 v8.0.72）对齐，0·J 欠账清零；Error 结构新增的 retry_after_seconds / credential_scoped 字段与 9 个标准错误码解锁，供后续错误码改造使用（本轮不改行为）。
3. **行为零变化的依据**：schema 7-10 的每一档都只对"声明了对应能力位"的插件生效，本树未声明任何新能力位（0·J 机会清单口径）；流块瘦身（schema 3/5 起）与管理响应免转义（6 起）在 v8.0.16（schema 6）时已生效。回归：全量 go vet / go test 通过，ship 门禁照常。
4. **断言口径已同步放宽**：build-ng.ps1 的钉版本断言从"必须等于 v8.0.16"放宽为"必须钉某个具体 v8.x 快照"（防未钉/跟 main，不再锁死具体值）；GOPROXY=off 保留（发版构建依旧离线密闭，可复现性由 go.sum 哈希保障）。

### 2026.11.14.1

**对齐轮（2026-11-13/14 全 fleet 审计）：过时测试注释改写为已闭环审计记录。**

### Fixed

1. `panel_test.go` 头部注释此前断言「scripts/panel-js-check.mjs 仍是 OXION 拷贝、本树门禁
   必红」+「dead-declaration 白名单条目待登记」——两项外部门禁事务**均已于 2026-11-13 落地**
   （脚本换成本树 22-id 骨架并实测通过；`config/api-keys/openai-compatibility` 已登记进
   dead-declaration.whitelist.json），注释却没跟着回改，读者会被误导以为门禁坏着（batch2
   审计抓出）。改写为「已闭环」审计记录，保留原判据与解法出处。

### 兼容性

无任何行为变更：只改注释文字。路由、面板、发现链一字未动。
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
