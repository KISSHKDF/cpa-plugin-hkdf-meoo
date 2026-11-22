// tree_hygiene_test.go — 树卫生守卫：构建产物绝不许留在源码树里。
//
// WHY THIS FILE EXISTS（一次被门禁拦下、三次记录在案的坑）
//
// 这棵树里反复出现同一个坑：**自检时跑了 `go build ./...`，在树根留下一个
// 25.9 MB 的 `workbuddy.exe`，随后每次发布都被 ship 的 `tree-no-artifacts`
// 判据拦下**，必须停下来删掉再重跑一次同一条命令。
//
// 已有的记录（说明这不是新坑，而是**我没记住的老坑**）：
//   - CHANGELOG 2026.11.10.2/.3 节：「`go build ./...` 在含 `package main`
//     的目录里会把可执行文件写到当前目录（本树即 `workbuddy.exe`）」；
//   - CHANGELOG 2026.11.19.x 节：「本轮自查时执行 `go build ./...` 在树根留下
//     24MB `workbuddy.exe`，被 ship 的 `tree-no-artifacts` 挡一次」；
//   - docs/reports/DELIVERY-2026.11.19.1.md：「已删除并记录为坑」。
//
// 为什么写了这个守卫还是又踩：**教训写进 CHANGELOG 是给人读的，但没人会在
// 每次 `go build` 前翻 CHANGELOG**。把它变成**测试**，才会在提交之前、
// 在我还没写 CHANGELOG 的时候当场报红 —— 那个时间点修起来只需删一个文件。
//
// 判据与发布门禁 `tree-no-artifacts`（release-check.ps1）同口径：
// 扫全树（build-out/ 除外）的 .dll / .h / .exe / .lib / .o / .a。
package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// treeArtifactExts 与 release-check.ps1 的 $artifactExt 保持一致。
//
// 为什么在这里重列一份而不是读 .gitignore：门禁注释里写明了原因
// （「a tree could otherwise edit its way out of the check by editing
// .gitignore」）—— 判据必须钉死在判据里，不能由被检查者自己定义。
var treeArtifactExts = []string{".dll", ".h", ".exe", ".lib", ".o", ".a"}

// TestTreeHasNoBuildArtifacts: 源码树里不许有任何构建产物。
//
// 失败信息必须**给出可执行的修复**，而不是只说"有产物"——
// 这个守卫的价值全在"当场告诉你怎么改"上。
func TestTreeHasNoBuildArtifacts(t *testing.T) {
	root := "."
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// build-out/ 是唯一被许可的树内产物目录（与门禁同口径）。
			if info.Name() == "build-out" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		for _, a := range treeArtifactExts {
			if ext == a {
				offenders = append(offenders, path+" ("+itoaSize(info.Size())+")")
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk tree: %v", err)
	}
	if len(offenders) == 0 {
		return
	}
	msg := "源码树里发现构建产物（发布门禁 tree-no-artifacts 会拦下，必须先清理再发版）：\n  " +
		strings.Join(offenders, "\n  ") +
		"\n\n这几乎总是同一个原因：自检时跑了 `go build ./...` —— " +
		"它在含 package main 的目录里会把可执行文件写到当前目录。\n" +
		"正确做法（任选其一）：\n" +
		"  1) 自检编译改用 `go vet ./...` 与 `go test -count=1 .`（它们不落产物）—— 推荐；\n" +
		"  2) 真要编译，产物必须显式指向树外：`go build -o \"$env:TEMP\\wb-check.exe\" .`；\n" +
		"  3) 正式构件一律走本树 scripts/build-ng.ps1（产物落 build-out/，门禁豁免）。\n" +
		"删掉上面的文件后重跑即可；**不要**为了让门禁变绿去改判据或改 .gitignore。"
	t.Fatal(msg)
}

// itoaSize renders a byte count in a human-readable form.
func itoaSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	whole := n / div
	frac := (n % div) * 10 / div
	units := []string{"KB", "MB", "GB", "TB"}
	if exp >= len(units) {
		exp = len(units) - 1
	}
	return strconv.FormatInt(whole, 10) + "." + strconv.FormatInt(frac, 10) + " " + units[exp]
}
