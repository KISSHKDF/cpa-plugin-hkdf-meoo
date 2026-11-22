package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestPluginLogoIsARenderablePngDataURI pins the brand-tile contract. The host
// renders metadata.Logo with <img src=...>, so the value has to be a data URI
// whose payload really is a PNG of usable size. Nothing else in this tree fails
// when it is blanked -- the management UI silently falls back to a generic plug
// icon -- nor when it is a truncated paste, which renders as a broken image.
// That silence is exactly why the assertion exists.
func TestPluginLogoIsARenderablePngDataURI(t *testing.T) {
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(pluginLogoURL, prefix) {
		t.Fatalf("pluginLogoURL must be a PNG data URI (len=%d)", len(pluginLogoURL))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pluginLogoURL, prefix))
	if err != nil {
		t.Fatalf("pluginLogoURL payload is not valid base64: %v", err)
	}
	magic := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if len(raw) < 24 || string(raw[:8]) != string(magic) {
		t.Fatalf("pluginLogoURL payload is not a PNG (len=%d)", len(raw))
	}
	w := int(raw[16])<<24 | int(raw[17])<<16 | int(raw[18])<<8 | int(raw[19])
	h := int(raw[20])<<24 | int(raw[21])<<16 | int(raw[22])<<8 | int(raw[23])
	if w < 32 || h < 32 {
		t.Fatalf("pluginLogoURL is %dx%d; the plugin list draws it at about 56px", w, h)
	}
}
