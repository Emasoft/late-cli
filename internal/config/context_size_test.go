package config

import "testing"

// TestModelSettingContextSizeOverride pins the context-size-tokens resolver:
// positive values override, zero/unset means "unknown — discovery only",
// and negative values are ignored (they cannot describe a real window).
func TestModelSettingContextSizeOverride(t *testing.T) {
	tests := []struct {
		name    string
		setting ModelSetting
		wantVal int
		wantOK  bool
	}{
		{"declared", ModelSetting{ContextSizeTokens: 32768}, 32768, true},
		{"unset", ModelSetting{}, 0, false},
		{"negative ignored", ModelSetting{ContextSizeTokens: -5}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.setting.ContextSizeOverride()
			if got != tt.wantVal || ok != tt.wantOK {
				t.Errorf("ContextSizeOverride() = (%d, %v), want (%d, %v)", got, ok, tt.wantVal, tt.wantOK)
			}
		})
	}
}

// TestParseConfigContent_ContextSizeTokensAccepted pins the strict parser's
// acceptance of the new per-model key and that the value survives the decode.
func TestParseConfigContent_ContextSizeTokensAccepted(t *testing.T) {
	content := `{"models": [{"id": "local", "url": "http://a:8080", "key": "", "model": "m", "context-size-tokens": 32768}]}`
	cfg, err := parseConfigContent("/x/config.json", []byte(content))
	if err != nil {
		t.Fatalf("parseConfigContent() error = %v", err)
	}
	if len(cfg.Models) != 1 {
		t.Fatalf("got %d models entries, want 1", len(cfg.Models))
	}
	if got := cfg.Models[0].ContextSizeTokens; got != 32768 {
		t.Errorf("ContextSizeTokens = %d, want 32768", got)
	}
}
