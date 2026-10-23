package config

import (
	"errors"
	"flag"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		mode Mode
		ids  []string
		err  string
	}{
		{name: "list", args: []string{"--list-all"}, mode: List},
		{name: "repeat IDs", args: []string{"--copy-item", "ABC!123", "--copy-item=DEF!456"}, mode: Copy, ids: []string{"ABC!123", "DEF!456"}},
		{name: "env list", env: map[string]string{"LIST_ALL": "true"}, mode: List},
		{name: "env copy", env: map[string]string{"COPY_ITEM_IDS": `["ABC!123","DEF!456"]`}, mode: Copy, ids: []string{"ABC!123", "DEF!456"}},
		{name: "CLI replaces invalid env action", args: []string{"--copy-item=ID"}, env: map[string]string{"LIST_ALL": "broken", "COPY_ITEM_IDS": "broken"}, mode: Copy, ids: []string{"ID"}},
		{name: "CLI list ignores copy env", args: []string{"--list-all"}, env: map[string]string{"COPY_ITEM_IDS": `["other"]`}, mode: List},
		{name: "list ignores Google config", args: []string{"--list-all"}, env: map[string]string{"GOOGLE_OAUTH_PORT": "broken"}, mode: List},
		{name: "opaque punctuation", args: []string{"--copy-item", `ID!"quotes',[]`}, mode: Copy, ids: []string{`ID!"quotes',[]`}},
		{name: "no action", err: "exactly one"},
		{name: "conflicting CLI", args: []string{"--list-all", "--copy-item=ID"}, err: "exactly one"},
		{name: "conflicting env", env: map[string]string{"LIST_ALL": "true", "COPY_ITEM_IDS": `["ID"]`}, err: "exactly one"},
		{name: "explicit false replaces env", args: []string{"--list-all=false"}, env: map[string]string{"LIST_ALL": "true"}, err: "exactly one"},
		{name: "unknown", args: []string{"--other"}, err: "not defined"},
		{name: "missing ID", args: []string{"--copy-item"}, err: "needs an argument"},
		{name: "flag instead of ID", args: []string{"--copy-item", "--list-all"}, err: "not a path"},
		{name: "empty ID", args: []string{"--copy-item="}, err: "not a path"},
		{name: "absolute path", args: []string{"--copy-item=/folder/file.txt"}, err: "not a path"},
		{name: "relative path", args: []string{"--copy-item=folder/file.txt"}, err: "not a path"},
		{name: "backslash path", args: []string{`--copy-item=folder\file.txt`}, err: "not a path"},
		{name: "URL", args: []string{"--copy-item=https://example.com"}, err: "not a path"},
		{name: "dot path", args: []string{"--copy-item=.."}, err: "not a path"},
		{name: "control", args: []string{"--copy-item=bad\nid"}, err: "control"},
		{name: "positional", args: []string{"file.txt"}, err: "positional"},
		{name: "trailing positional", args: []string{"--list-all", "file.txt"}, err: "positional"},
		{name: "bad boolean", env: map[string]string{"LIST_ALL": "yes"}, err: "boolean"},
		{name: "bad JSON", env: map[string]string{"COPY_ITEM_IDS": `["ID"`}, err: "JSON array"},
		{name: "not an array", env: map[string]string{"COPY_ITEM_IDS": `"ID"`}, err: "JSON array"},
		{name: "null array", env: map[string]string{"COPY_ITEM_IDS": `null`}, err: "JSON array"},
		{name: "nonstring array", env: map[string]string{"COPY_ITEM_IDS": `[42]`}, err: "JSON array"},
		{name: "null item", env: map[string]string{"COPY_ITEM_IDS": `[null]`}, err: "not a path"},
		{name: "missing app", args: []string{"--list-all"}, env: map[string]string{"MICROSOFT_CLIENT_ID": ""}, err: "MICROSOFT_CLIENT_ID"},
		{name: "bad interactive", args: []string{"--list-all"}, env: map[string]string{"AUTH_INTERACTIVE": "sometimes"}, err: "boolean"},
		{name: "bad port", args: []string{"--copy-item=ID"}, env: map[string]string{"GOOGLE_OAUTH_PORT": "65536"}, err: "1 to 65535"},
		{name: "bad host", args: []string{"--copy-item=ID"}, env: map[string]string{"GOOGLE_OAUTH_LISTEN_HOST": "example.com"}, err: "127.0.0.1"},
		{name: "root state", args: []string{"--list-all"}, env: map[string]string{"AUTH_DIR": "/"}, err: "filesystem root"},
		{name: "shared directories", args: []string{"--copy-item=ID"}, env: map[string]string{"AUTH_DIR": "same", "TEMP_COPY_DIR": "same"}, err: "separate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"MICROSOFT_CLIENT_ID": "test-app"}
			for k, v := range tt.env {
				env[k] = v
			}
			cfg, err := Parse(tt.args, func(k string) string { return env[k] })
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %v, want containing %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mode != tt.mode || !reflect.DeepEqual(cfg.ItemIDs, tt.ids) {
				t.Errorf("action = %s %v, want %s %v", cfg.Mode, cfg.ItemIDs, tt.mode, tt.ids)
			}
			if !filepath.IsAbs(cfg.AuthDir) || !filepath.IsAbs(cfg.TempCopyDir) {
				t.Error("runtime paths must be absolute")
			}
		})
	}
}

func TestHelpNeedsNoConfiguration(t *testing.T) {
	_, err := Parse([]string{"--help"}, func(string) string {
		t.Fatal("help must not read credentials or environment")
		return ""
	})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("error = %v, want help", err)
	}
}
