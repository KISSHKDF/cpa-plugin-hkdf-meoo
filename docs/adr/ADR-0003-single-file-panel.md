# ADR-0003 面板是单文件 panel.html + //go:embed，不是前端工程

- 状态：已采纳
- 日期：2026-10-23（成文）
- 相关版本：全部

## 背景

CPA 宿主把插件面板渲染成一个**全屏 iframe**（`PluginResourcePage`，
`width/height:100%`、`border:0`、`padding:0`，与宿主同源、无 sandbox），
插件通过 `management.register` 的 `resources` 字段声明一个 URL，
宿主把那个 URL 塞进 iframe。

也就是说：面板就是一个**独立网页**，不需要和宿主的 React 应用共享任何运行时。

## 决策

**每个插件只有一份 `panel.html`**（内联 `<style>` + `<script>`），通过 `//go:embed`
编进 DLL，由插件的 management 路由直接返回字节。

- 无构建步骤（没有 webpack/vite/tsc）
- 无 npm 依赖
- 无 node_modules
- 面板与 Go 代码同一个提交、同一个版本号

## 被否掉的选项

| 选项 | 为什么否掉 |
|---|---|
| React/Vue + 构建流水线 | 面板只有一个页面、几个列表；引入构建链意味着多一套工具链、多一份 node_modules、发版时多一步"先 build 前端再 build Go"，出错面显著变大 |
| 从 CDN 引框架 | 面板在本地 CPA 里跑，且用户环境可能离线；外部依赖是不可接受的失败点 |
| 面板做成独立静态站点由 CPA 托管 | 宿主没有这个能力，且会引入跨域/路径前缀问题 |

## 后果

- **好的**：一个插件 = 一个 git 仓库 + 一个 Go 包 + 一份 HTML；`go build` 就是全部构建；
  面板改动和 Go 改动在同一个 commit 里，不可能版本错配。
- **代价**：
  - HTML 会很长（workbuddy 的 `panel.html` 现在 12 万字符）——**靠测试守**：
    各树有 `panel_*_test.go` / `marker_audit_test.go` 检查面板里该有的元素与文案
  - 没有类型检查，只能靠 `panel-syntax-check.mjs`（抽出 `<script>` 用 `vm.Script` 编译）+ 无头浏览器渲染验证
  - 样式与宿主主题同步要手写（各面板监听 `MutationObserver` + `prefers-color-scheme`）
- **什么时候该重新考虑**：如果某个插件的面板复杂到需要组件化（例如超过 3 个交互视图、
  需要状态管理），那时单独为它引一套构建链，并写新 ADR 说明为何破例。

## 证据

- `CONVENTIONS.md` 第二节：`panel.html` 是根目录允许的例外（`//go:embed` 目标）
- 各树 `panel.html` 顶部注释：主题 token 对齐 CPA 管理面板
- `C:\CLIP\_multi-plugins-src\panel-syntax-check.mjs`：抽出 `<script>` 编译检查
- 宿主侧：`PluginResourcePage.tsx` 的 `<iframe className={styles.frame} … />`（无 sandbox）
- 2026-10-23 toast 位置修复即此 ADR 的直接后果：iframe 内 z-index 被层叠上下文锁死，
  只能靠改 CSS 位置避让宿主按钮条
