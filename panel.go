// panel.go serves the browser UI that the host renders as the "Oxion"
// card under CPA's 插件 menu.
//
// Mechanism (read out of the running host, not assumed): the host has no panel
// concept at all — internal/pluginhost contains zero references to "panel". A
// plugin gets a card simply by declaring a *resource* route with a Menu label in
// management.register, and answering GET on that route with text/html. The host
// embeds it in an iframe under /<door>/resource/plugins/<id>/panel, and the page
// then talks to /<door>/management/plugins/<id>/… with the management key the
// control panel already keeps in localStorage.
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
const panelMenuLabel = "Oxion"

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

func loadedResourceBasePath() string {
	resourceBasePathMu.RLock()
	defer resourceBasePathMu.RUnlock()
	return strings.TrimRight(resourceBasePathCache, "/")
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

// handlePublishedModels feeds two consumers at once:
//
//   - the panel's catalogue strip, which wants a flat `models` list;
//   - the model enable/disable card, which wants the sibling trees' `groups`
//     shape (one group per channel, each carrying its model list). Keeping both
//     in one response means the panel makes one call, and the "disabled" state
//     it renders can never disagree with the list it just drew.
//
// `groups` carries the RAW catalogue (every advertisable id), not the filtered
// one: the picker has to show a disabled model so it can be re-enabled. The
// host-side filter is what actually stops routing to it.
func handlePublishedModels(req pluginapi.ManagementRequest) map[string]any {
	_ = req
	sa, _, ok := firstCredential()
	ids := snapshotIDs()
	source := "snapshot"
	if ok {
		ids, source, _ = discoverModels(sa)
	}
	out := make([]map[string]any, 0, len(ids))
	group := make([]map[string]any, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if !advertisable(id) || seen[id] {
			continue
		}
		seen[id] = true
		published := advertisedName(id)
		entry := map[string]any{"id": published, "upstream": id}
		if m, found := snapshotByID(id); found {
			entry["name"] = m.Name
			entry["context"] = m.Context
		}
		out = append(out, entry)
		group = append(group, entry)
	}
	// The HOST list, not the plugin's own excluded_models config: the picker
	// renders the host list, so the count beside it must come from the same place.
	excluded, excludedSeen := hostExcludedSnapshot()
	return map[string]any{
		"provider":       providerName,
		"source":         source,
		"total_upstream": len(ids),
		"models":         out,
		"groups": []map[string]any{{
			"realm":    providerName,
			"label":    "Oxion",
			"source":   source,
			"count":    len(group),
			"models":   group,
			"excluded": excluded,
		}},
		"excluded":      excluded,
		"excluded_seen": excludedSeen,
	}
}

// firstCredential returns the first usable stored credential, for catalogue calls
// that need one (the published list is per-account on this gateway).
func firstCredential() (*storedAuth, string, bool) {
	entries, err := hostAuthList()
	if err != nil {
		return nil, "", false
	}
	for _, entry := range entries {
		if sa, ok := lookupCredential(entry.AuthIndex, entry.Name); ok && strings.TrimSpace(sa.APIKey) != "" {
			return sa, entry.AuthIndex, true
		}
	}
	return nil, "", false
}
