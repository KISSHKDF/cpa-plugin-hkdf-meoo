// panel.go serves the browser UI the host renders as the "Meoo 秒悟" card under
// CPA's 插件 menu.
//
// Mechanism (read out of the running host, not assumed): the host has no panel
// concept at all — internal/pluginhost contains zero references to "panel". A
// plugin gets a card simply by declaring a *resource* route with a Menu label in
// management.register, and answering GET on that route with text/html. The host
// embeds it in an iframe under /<door>/resource/plugins/<id>/panel, and the page
// then talks to /<door>/management/plugins/<id>/… with the management key the
// control panel already keeps in localStorage.
//
// (This file was copied from the oxion tree — same single-file embed, same door
// regex, same raw-HTML response — and had the oxion-only model-catalogue handler
// removed: Meoo's catalogue lives in management.go/modelsView.)
package main

import (
	_ "embed" // required by //go:embed panel.html
	"net/http"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed panel.html
var panelHTML []byte

// panelMenuLabel is the text CPA shows on the plugin card.
const panelMenuLabel = "Meoo 秒悟"

// resourceDoorRE accepts whichever door the host injected (v8 keeps /v0 alive and
// advertises /v8; hardcoding one of them 404s on the other).
var resourceDoorRE = regexp.MustCompile(`^/v\d+/resource/plugins/` + regexp.QuoteMeta(providerName))

// resourceSubPath reports whether a management call is actually a browser
// resource request for this plugin's panel, and what the sub-path is.
func resourceSubPath(path string) (sub string, ok bool) {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	base := loadedResourceBasePath()
	if base != "" {
		if trimmed == base || strings.HasPrefix(trimmed, base+"/") {
			return strings.TrimPrefix(trimmed, base), true
		}
	}
	if loc := resourceDoorRE.FindStringIndex(trimmed); loc != nil && loc[0] == 0 {
		return strings.TrimPrefix(trimmed, resourceDoorRE.FindString(trimmed)), true
	}
	return "", false
}

// loadedResourceBasePath is the browser-facing prefix, with the same safe default
// the host's own registration uses (the host may never tell us).
func loadedResourceBasePath() string {
	resourceBasePathMu.RLock()
	cached := resourceBasePathCache
	resourceBasePathMu.RUnlock()
	if cached != "" {
		return cached
	}
	return "/v0/resource/plugins/" + providerName
}

// servePanel answers the resource route. Anything other than the panel itself is
// a 404 page rather than a silent empty 200.
func servePanel(sub string) []byte {
	switch strings.TrimSpace(sub) {
	case "", "/", "/panel", "/panel.html", "/index.html":
		return panelHTML
	default:
		return []byte("<!DOCTYPE html><html lang=\"zh-CN\"><body style=\"font:14px system-ui;padding:24px\">未知面板路径 " +
			htmlEscape(sub) + "（本插件只有一张卡片：<code>/panel</code>）</body></html>")
	}
}

// mgmtHTMLResponse wraps raw HTML in the ONE place the browser looks for the
// content type. The outer RPC envelope carries this struct; the HTML is the Body,
// never a JSON document that happens to contain HTML.
func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	headers := http.Header{}
	headers.Set("Content-Type", "text/html; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: body}
}

// htmlEscape is the Go-side twin of the panel's esc(); the error page above is
// built here, so it must escape too rather than interpolate a raw URL path.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
