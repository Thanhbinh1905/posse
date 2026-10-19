package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

const vocabularyMarker = "# posse vocabulary: 1\n"

// ReplacementKey is shared by validation and the one-time stored-config upgrade.
func ReplacementKey(key string) string {
	switch key {
	case "remuda":
		return "pool"
	case "defaults.landing_mode":
		return "defaults.merge_mode"
	case "defaults.auto_unsaddle":
		return "defaults.auto_teardown"
	case "autonomy.land":
		return "autonomy.merge"
	}
	if strings.HasPrefix(key, "remuda.") {
		return "pool." + strings.TrimPrefix(key, "remuda.")
	}
	if strings.HasPrefix(key, "repositories.") && strings.HasSuffix(key, ".landing_mode") && len(key) > len("repositories.")+len(".landing_mode") {
		return strings.TrimSuffix(key, ".landing_mode") + ".merge_mode"
	}
	return ""
}

func unknownKeyReason(key string) string {
	if replacement := ReplacementKey(key); replacement != "" {
		return "was removed; use " + replacement
	}
	return "unknown configuration key"
}

// migrateVocabularyFile upgrades stored 0.14 config exactly once, before it is
// validated. Subsequent legacy input is an error, not a working alias. Preserve
// the original bytes next to the file for rollback, including comments.
func migrateVocabularyFile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || bytes.HasPrefix(data, []byte(vocabularyMarker)) {
		return err
	}
	if marker, err := os.ReadFile(path + ".vocabulary"); err == nil && string(marker) == vocabularyMarker {
		return nil
	}
	lock, err := os.OpenFile(path+".vocabulary-lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	data, err = os.ReadFile(path)
	if err != nil || bytes.HasPrefix(data, []byte(vocabularyMarker)) {
		return err
	}
	values := map[string]any{}
	if _, err := toml.Decode(string(data), &values); err != nil {
		return &InvalidError{File: path, Key: tomlErrorKey(err), Cause: err}
	}
	changed, err := migrateVocabularyMap(values, "")
	if err != nil {
		return &InvalidError{File: path, Key: "config", Cause: err}
	}
	if !changed {
		return atomicfile.Write(path+".vocabulary", []byte(vocabularyMarker), 0o600)
	}
	updated := append([]byte(vocabularyMarker), data...)
	if changed {
		var encoded bytes.Buffer
		encoded.WriteString(vocabularyMarker)
		if err := toml.NewEncoder(&encoded).Encode(values); err != nil {
			return err
		}
		updated = encoded.Bytes()
	}
	if err := atomicfile.Write(path+".v014", data, filePerm(path)); err != nil {
		return err
	}
	return atomicfile.Write(path, updated, filePerm(path))
}

func migrateVocabularyMap(values map[string]any, prefix string) (bool, error) {
	changed := false
	for key, value := range values {
		full := key
		if prefix != "" {
			full = prefix + "." + key
		}
		target := ReplacementKey(full)
		if target != "" {
			newKey := target[strings.LastIndex(target, ".")+1:]
			if newKey != key {
				if _, exists := values[newKey]; exists {
					return false, fmt.Errorf("both %s and its replacement %s are present; keep %s", full, target, target)
				}
				delete(values, key)
				values[newKey], key, full, changed = value, newKey, target, true
			}
		}
		if table, ok := value.(map[string]any); ok {
			childChanged, err := migrateVocabularyMap(table, full)
			if err != nil {
				return false, err
			}
			changed = changed || childChanged
		}
		if full == "dispatch" {
			if rules, ok := tableEntries(value); ok {
				for _, rule := range rules {
					if replacement := legacyTaskType(rule["type"]); replacement != "" {
						rule["type"], changed = replacement, true
					}
				}
			}
		}
		if strings.HasPrefix(full, "kinds.") && key == "notice_delivery" && value == "lookout" {
			values[key], changed = "watch", true
		}
		if key == "auto_teardown" && value == "landed" {
			values[key], changed = "merged", true
		}
		if full == "identity.worker.display_prefix" && value == "rider" {
			values[key], changed = "worker", true
		}
	}
	return changed, nil
}

func legacyTaskType(value any) string {
	switch value {
	case "ship":
		return "change"
	case "scout":
		return "research"
	}
	return ""
}
