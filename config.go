// config.go — plugin configuration: the refresh window, the provisioning switch
// and the project name.
//
// ---------------------------------------------------------------------------
// Why a struct with yaml tags (and not a map)
// ---------------------------------------------------------------------------
//
// The release gate's dead-declaration check proves a declared ConfigField is
// actually CONSUMED by walking a ladder: the key must bind at a parse site, and
// the bound symbol must be READ somewhere. A struct-tag binding is one of the
// recognised binding sites; a key composed at runtime is not, and the check then
// reports DECLARED_NOT_PARSED — correctly.
//
// Pointer fields exist so "absent" is distinguishable from "false"/"0"/"". Absent
// means: enabled, 60 minutes, auto-provision OFF, project name CPA-Model-Gateway.
//
// ---------------------------------------------------------------------------
// `enabled` is parsed but deliberately NOT declared
// ---------------------------------------------------------------------------
//
// The agreed config surface for this plugin is the three keys in
// meooConfigFields. `enabled` is still read from the same YAML block because the
// host config may carry it, the panel echoes it and /health reports it; leaving
// it unparsed would mean a config that says enabled:false still swept upstream.
// Guilding the declaration would widen a frozen surface, so it stays undeclared.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// refreshMinutesDefault is the agreed behaviour: the panel's readings may be up
// to an hour old unless the operator presses 刷新数据. Every upstream read in
// this contract is a read-only query (the two provisioning POSTs are gated by
// auto_provision — default OFF — and only fire once per account), so the window
// is about politeness and latency, not points.
const (
	refreshMinutesDefault = 60
	refreshMinutesMin     = 1
	refreshMinutesMax     = 1440
)

// projectNameDefault names a project this plugin creates for an account that has
// none. Kept identical to the name of the project the operator already runs, so
// an auto-provisioned row is recognisable in the Meoo console.
const projectNameDefault = "CPA-Model-Gateway"

// projectNameMaxLen bounds the name handed to the create-project POST: the
// upstream rejects an over-long name, and a silently truncated project would be
// worse than a refused one.
const projectNameMaxLen = 64

// parsedSwitches is the raw YAML view. Pointers so absent != false/0/"".
type parsedSwitches struct {
	Enabled        *bool   `yaml:"enabled"`
	RefreshMinutes *int    `yaml:"refresh_minutes"`
	AutoProvision  *bool   `yaml:"auto_provision"`
	ProjectName    *string `yaml:"project_name"`
	DailyLogin     *bool   `yaml:"daily_login"`
}

// pluginConfig is the effective configuration after defaults are applied.
type pluginConfig struct {
	Enabled        bool
	RefreshMinutes int
	AutoProvision  bool
	ProjectName    string
	// DailyLogin enables the daily 2,000-point login reward (daily_login.go).
	//
	// It is ON by default because the operator asked for it, and it is a switch
	// at all because the feature reads a browser profile on this machine: turning
	// it off must be possible without uninstalling the plugin.
	DailyLogin bool
}

// defaultConfig is the state before any register call.
//
// AutoProvision defaults OFF (2026-11-22 operator ruling: unrequested automatic
// upstream writes are never acceptable) — a fresh install walks READ-ONLY and
// provisioning must be an explicit operator decision.
func defaultConfig() pluginConfig {
	return pluginConfig{
		Enabled:        true,
		RefreshMinutes: refreshMinutesDefault,
		AutoProvision:  false,
		ProjectName:    projectNameDefault,
		DailyLogin:     true,
	}
}

var (
	cfgMu      sync.RWMutex
	currentCfg = defaultConfig()
)

// configure applies one register/reconfigure envelope.
//
// The host hands us the plugin's whole config BLOCK, whose own top-level key is
// `config_yaml` holding the body as a literal block scalar — so the first parse
// yields a mapping with a `config_yaml` key and no plugin keys at all. Detect
// that shape and unwrap one more level. (Same trap the fleet's other trees
// document: every plugin key looked "absent" and nothing was ever applied.)
func configure(raw []byte) {
	next := defaultConfig()

	// The host marshals ConfigYAML as a Go []byte, which encoding/json emits as
	// BASE64. A second decode with a plain string field is attempted because the
	// same key has also been observed carrying readable YAML; accepting both costs
	// nothing and removes a silent "config was never applied" failure mode.
	var asBytes struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if errUnmarshal := json.Unmarshal(raw, &asBytes); errUnmarshal == nil && len(asBytes.ConfigYAML) > 0 {
		applySwitches(asBytes.ConfigYAML, &next)
	} else {
		var asString struct {
			ConfigYAML string `json:"config_yaml"`
		}
		if errString := json.Unmarshal(raw, &asString); errString == nil && strings.TrimSpace(asString.ConfigYAML) != "" {
			applySwitches([]byte(asString.ConfigYAML), &next)
		}
	}

	cfgMu.Lock()
	currentCfg = next
	cfgMu.Unlock()
}

// applySwitches decodes one YAML body into next, unwrapping the outer block shape
// if that is what we were handed.
func applySwitches(body []byte, next *pluginConfig) {
	var direct parsedSwitches
	if errYAML := yaml.Unmarshal(body, &direct); errYAML != nil {
		return
	}
	// The outer-block shape: the body sits under a literal `config_yaml` scalar.
	if direct.Enabled == nil && direct.RefreshMinutes == nil && direct.AutoProvision == nil && direct.ProjectName == nil && direct.DailyLogin == nil {
		var outer map[string]any
		if errOuter := yaml.Unmarshal(body, &outer); errOuter == nil {
			if inner, okInner := outer["config_yaml"].(string); okInner && strings.TrimSpace(inner) != "" {
				var nested parsedSwitches
				if errNested := yaml.Unmarshal([]byte(inner), &nested); errNested == nil {
					direct = nested
				}
			}
		}
	}
	if direct.Enabled != nil {
		next.Enabled = *direct.Enabled
	}
	if direct.RefreshMinutes != nil {
		next.RefreshMinutes = clampRefreshMinutes(*direct.RefreshMinutes)
	}
	if direct.AutoProvision != nil {
		next.AutoProvision = *direct.AutoProvision
	}
	if direct.ProjectName != nil {
		next.ProjectName = clampProjectName(*direct.ProjectName)
	}
	if direct.DailyLogin != nil {
		next.DailyLogin = *direct.DailyLogin
	}
}

// clampRefreshMinutes bounds the window. A zero or negative value would make the
// TTL gate always expire (an upstream read per panel poll) and a huge value would
// freeze the panel on a day-old reading; both are refused rather than honoured.
func clampRefreshMinutes(n int) int {
	if n < refreshMinutesMin {
		return refreshMinutesMin
	}
	if n > refreshMinutesMax {
		return refreshMinutesMax
	}
	return n
}

// clampProjectName refuses a name the create-project POST would reject. An empty
// or whitespace-only value falls back to the default rather than creating a
// project called "".
//
// 2026-11-20: the cap is a BYTE cap (the upstream limit), but it is now applied at
// a rune boundary. A project name is operator-typed text and is routinely Chinese;
// `trimmed[:64]` split the last rune, so the name this plugin POSTed was not valid
// UTF-8 (and the persisted project name came back as a replacement character).
// Same 64-byte ceiling, never an over-long name: strictly more conservative.
func clampProjectName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return projectNameDefault
	}
	if len(trimmed) > projectNameMaxLen {
		return truncateUTF8Bytes(trimmed, projectNameMaxLen)
	}
	return trimmed
}

// currentRefreshTTL is the cache lifetime / background period.
func currentRefreshTTL() time.Duration {
	cfgMu.RLock()
	minutes := currentCfg.RefreshMinutes
	cfgMu.RUnlock()
	return time.Duration(clampRefreshMinutes(minutes)) * time.Minute
}

// currentEnabled reports whether the sweep should run.
func currentEnabled() bool {
	cfgMu.RLock()
	enabled := currentCfg.Enabled
	cfgMu.RUnlock()
	return enabled
}

// currentAutoProvision reports whether the walk may create a project (POST
// /cli-compat/projects) and enable its cloud instance (POST
// /cli-compat/projects/{url_id}/cloud) for an account that has none. Read by
// resolveProject in upstream.go, once per account per walk.
func currentAutoProvision() bool {
	cfgMu.RLock()
	allowed := currentCfg.AutoProvision
	cfgMu.RUnlock()
	return allowed
}

// currentProjectName is the name handed to the create-project POST. Read by
// resolveProject in upstream.go.
func currentProjectName() string {
	cfgMu.RLock()
	name := currentCfg.ProjectName
	cfgMu.RUnlock()
	return clampProjectName(name)
}

// currentDailyLogin reports whether the daily 2,000-point login reward may run.
//
// Read by the daily-login sweep (daily_login.go). The switch exists because the
// feature reads a browser profile on this machine; the operator must be able to
// turn it off without uninstalling the plugin.
func currentDailyLogin() bool {
	cfgMu.RLock()
	enabled := currentCfg.DailyLogin
	cfgMu.RUnlock()
	return enabled
}

// configEcho renders the effective config for the panel (no secrets here).
func configEcho() map[string]any {
	cfgMu.RLock()
	cfg := currentCfg
	cfgMu.RUnlock()
	return map[string]any{
		"enabled":         cfg.Enabled,
		"refresh_minutes": cfg.RefreshMinutes,
		"auto_provision":  cfg.AutoProvision,
		"project_name":    clampProjectName(cfg.ProjectName),
		"daily_login":     cfg.DailyLogin,
	}
}

// timeAfter is the clock seam for the refresh loop (tests replace it through
// timeAfterFn). It is read under a mutex because the refresh goroutine reads it on
// every iteration while a test may be replacing it: a bare variable here is a data
// race the -race gate would (correctly) fail.
var (
	timeAfterMu sync.Mutex
	timeAfterFn = func(d time.Duration) <-chan time.Time { return time.After(d) }
)

// timeAfter waits for the configured clock.
func timeAfter(d time.Duration) <-chan time.Time {
	timeAfterMu.Lock()
	fn := timeAfterFn
	timeAfterMu.Unlock()
	return fn(d)
}

// setTimeAfterForTest lives in accounts_test.go (production code must not import
// the testing package).

// resetConfigForTest reinstates the default.
func resetConfigForTest() {
	cfgMu.Lock()
	currentCfg = defaultConfig()
	cfgMu.Unlock()
}
