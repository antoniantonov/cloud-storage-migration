package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const Usage = `Usage: cloud-storage-migration --list-all
       cloud-storage-migration --copy-item ID [--copy-item ID ...]

  --list-all       List all live OneDrive descendants as JSON Lines.
  --copy-item ID   Copy a regular file by OneDrive ID (not a path or folder).
  --help          Show this help without authenticating.

Without action flags, use LIST_ALL=true or COPY_ITEM_IDS='["ID", "ID2"]'.
MICROSOFT_CLIENT_ID is required. Copies also use GOOGLE_CLIENT_CREDENTIALS_FILE.
See README.md for registered OAuth apps, persistent tokens, and Docker options.
`

type itemIDs []string

func (i *itemIDs) String() string { return strings.Join(*i, ",") }
func (i *itemIDs) Set(value string) error {
	if err := ValidateID(value); err != nil {
		return err
	}
	*i = append(*i, value)
	return nil
}

func ValidateID(id string) error {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." ||
		strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, "-") ||
		strings.ContainsRune(id, ':') {
		return errors.New("--copy-item requires a OneDrive item ID, not a path, URL, or flag")
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return errors.New("OneDrive item IDs must not contain control characters")
		}
	}
	return nil
}

func Parse(args []string, getenv func(string) string) (Config, error) {
	var cfg Config
	var list bool
	var ids itemIDs
	flags := flag.NewFlagSet("cloud-storage-migration", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&list, "list-all", false, "")
	flags.Var(&ids, "copy-item", "")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional argument; use --copy-item with an item ID")
	}
	hasActionFlag := false
	flags.Visit(func(*flag.Flag) { hasActionFlag = true })
	if !hasActionFlag {
		var err error
		list, err = boolEnv(getenv, "LIST_ALL", false)
		if err != nil {
			return cfg, err
		}
		if raw := getenv("COPY_ITEM_IDS"); raw != "" {
			if !strings.HasPrefix(strings.TrimSpace(raw), "[") {
				return cfg, errors.New("COPY_ITEM_IDS must be a JSON array of item-ID strings")
			}
			if err := json.Unmarshal([]byte(raw), &ids); err != nil {
				return cfg, errors.New("COPY_ITEM_IDS must be a JSON array of item-ID strings")
			}
		}
	}
	for _, id := range ids {
		if err := ValidateID(id); err != nil {
			return cfg, err
		}
	}
	if list == (len(ids) > 0) {
		return cfg, errors.New("select exactly one action: --list-all or one or more --copy-item IDs")
	}
	cfg.Mode = List
	if len(ids) > 0 {
		cfg.Mode = Copy
	}
	cfg.ItemIDs = ids
	cfg.MicrosoftClientID = strings.TrimSpace(getenv("MICROSOFT_CLIENT_ID"))
	if cfg.MicrosoftClientID == "" {
		return cfg, errors.New("MICROSOFT_CLIENT_ID must identify your registered Microsoft public-client app")
	}
	var err error
	cfg.InteractiveAuth, err = boolEnv(getenv, "AUTH_INTERACTIVE", true)
	if err != nil {
		return cfg, err
	}
	cfg.AuthDir, err = absoluteDir(valueOr(getenv("AUTH_DIR"), ".auth"))
	if err != nil {
		return cfg, fmt.Errorf("AUTH_DIR: %w", err)
	}
	cfg.TempCopyDir, err = absoluteDir(valueOr(getenv("TEMP_COPY_DIR"), "temp-copy"))
	if err != nil {
		return cfg, fmt.Errorf("TEMP_COPY_DIR: %w", err)
	}
	cfg.GoogleOAuthPort = 8765
	cfg.GoogleOAuthListenHost = "127.0.0.1"
	if cfg.Mode == Copy {
		if cfg.AuthDir == cfg.TempCopyDir {
			return cfg, errors.New("AUTH_DIR and TEMP_COPY_DIR must be separate directories")
		}
		if file := getenv("GOOGLE_CLIENT_CREDENTIALS_FILE"); file != "" {
			cfg.GoogleCredentialsFile, err = filepath.Abs(file)
			if err != nil {
				return cfg, fmt.Errorf("resolve GOOGLE_CLIENT_CREDENTIALS_FILE: %w", err)
			}
		}
		if port := getenv("GOOGLE_OAUTH_PORT"); port != "" {
			cfg.GoogleOAuthPort, err = strconv.Atoi(port)
			if err != nil || cfg.GoogleOAuthPort < 1 || cfg.GoogleOAuthPort > 65535 {
				return cfg, errors.New("GOOGLE_OAUTH_PORT must be an integer from 1 to 65535")
			}
		}
		cfg.GoogleOAuthListenHost = valueOr(getenv("GOOGLE_OAUTH_LISTEN_HOST"), "127.0.0.1")
		if cfg.GoogleOAuthListenHost != "127.0.0.1" && cfg.GoogleOAuthListenHost != "0.0.0.0" {
			return cfg, errors.New("GOOGLE_OAUTH_LISTEN_HOST must be 127.0.0.1 (native) or 0.0.0.0 (container)")
		}
	}
	return cfg, nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func boolEnv(getenv func(string) string, name string, fallback bool) (bool, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	result, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return result, nil
}

func absoluteDir(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if filepath.Dir(absolute) == absolute {
		return "", errors.New("must not be the filesystem root")
	}
	return absolute, nil
}
