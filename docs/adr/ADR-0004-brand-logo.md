# ADR-0004：插件头像用 PNG data URI，不用外链 URL

- 日期：2026-11-13
- 状态：已采纳

## 背景

宿主的管理界面在插件列表里用 `<img src=...>` 画 `metadata.Logo`（`internal/api/handlers/management/plugins.go`
把该字段透出，`plugins_test.go` 还专门锁了它必须被 HTML 转义）。此前本插件的
`pluginLogoURL` 是空串，界面于是回落到那枚通用"插头"占位图标 —— 在一列插件里认不出谁是谁。

`Logo` 是字符串类型（`sdk/pluginapi/types.go`："a plugin-provided display asset reference
for management clients"），宿主只做转义、不做 URL 校验，所以有三种填法：留空、填 https 外链、
填 data URI。

## 决策

**填 PNG data URI。**

## 理由

1. **管理界面是从本机 localhost 提供的**。填外链意味着每次打开插件列表，浏览器都要往第三方
   CDN 发一次请求才能画出一枚 56px 的小图标 —— 为了一个装饰性资源，把界面的可用性押在网络上，
   并且顺带把"本机在用什么插件"暴露给第三方。
2. **离线也要能画**。本机常态是断网/走代理的环境，外链挂了就退回占位图标，等于没配。
3. **代价可接受**：64x64 的 PNG 只有几 KB，base64 后写进 `logo.go` 一个常量，跟着 DLL 走，
   没有额外文件、没有运行时 IO。

## 不做外链的负面后果（已接受）

- 源图更新后需要重新生成常量并发新版本（本仓库本来就走版本化发布，成本可忽略）。
- `logo.go` 里有一行很长的 base64 —— 因此把来源、变换步骤与哈希单独记在本 ADR 里，
  而不是靠那一行自证。

## 素材来源与变换

见本目录下各版本的 `logo.go` 注释；每次重新生成必须在下面追加一行：

| 生成日期 | 源 | 变换 | 64x64 PNG SHA256 |
|---|---|---|---|
| 2026-11-13 | `https://gw.alicdn.com/imgextra/i4/O1CN01MxUXzS1xYRjMwA2la_!!6000000006455-2-tps-184-184.png`（meoo.com 首页 `<link rel="icon">` 声明的那枚，184x184） | PNG → 高质量双三次缩到 64x64，透明底 | A3FD76F7B947E8C42036DE0974A104302240EEED48F2D3F3DCB2214CE45F6179 |

## 使用性质

秒悟 Meoo 是第三方产品。这里只把它官方站点图标用于"识别这是哪个通道"（指明性使用），不再分发、不主张任何权利；若权利方要求，删掉 `logo.go` 即恢复占位图标。