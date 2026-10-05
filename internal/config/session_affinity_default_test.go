package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionAffinityDefaultsToEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{name: "routing omitted", yaml: "port: 8317\n", want: true},
		{name: "routing without affinity key", yaml: "routing:\n  strategy: fill-first\n", want: true},
		{name: "explicit true", yaml: "routing:\n  session-affinity: true\n", want: true},
		{name: "explicit false", yaml: "routing:\n  session-affinity: false\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tc.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.Routing.SessionAffinityEnabled(); got != tc.want {
				t.Fatalf("SessionAffinityEnabled() = %v, want %v", got, tc.want)
			}

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(configPath, []byte(tc.yaml), 0o600); errWrite != nil {
				t.Fatalf("os.WriteFile() error = %v", errWrite)
			}
			loaded, errLoad := LoadConfigOptional(configPath, false)
			if errLoad != nil {
				t.Fatalf("LoadConfigOptional() error = %v", errLoad)
			}
			if got := loaded.Routing.SessionAffinityEnabled(); got != tc.want {
				t.Fatalf("loaded SessionAffinityEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSaveConfigPreserveCommentsPersistsExplicitSessionAffinityFalse(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatalf("os.WriteFile() error = %v", errWrite)
	}

	// Saving the default (unset) value must not add a routing block.
	if errSave := SaveConfigPreserveComments(configPath, &Config{Port: 8317}); errSave != nil {
		t.Fatalf("SaveConfigPreserveComments(default) error = %v", errSave)
	}
	savedDefault, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("os.ReadFile() error = %v", errRead)
	}
	if strings.Contains(string(savedDefault), "session-affinity") {
		t.Fatalf("default save wrote session-affinity:\n%s", savedDefault)
	}

	// An explicit false is a real choice now that the default is true, so it must be written.
	disabled := false
	if errSave := SaveConfigPreserveComments(configPath, &Config{
		Port:    8317,
		Routing: RoutingConfig{SessionAffinity: &disabled},
	}); errSave != nil {
		t.Fatalf("SaveConfigPreserveComments(false) error = %v", errSave)
	}
	savedFalse, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("os.ReadFile() error = %v", errRead)
	}
	if !strings.Contains(string(savedFalse), "session-affinity: false") {
		t.Fatalf("explicit false was not persisted:\n%s", savedFalse)
	}
	loaded, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	if loaded.Routing.SessionAffinityEnabled() {
		t.Fatal("reloaded config re-enabled session affinity after an explicit false was saved")
	}
}
