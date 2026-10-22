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

const (
	vocabularyMarkerV1 = "# posse vocabulary: 1\n"
	vocabularyMarker   = "# posse vocabulary: 2\n"
)

func vocabularyMarkerVersion(data []byte) int {
	switch {
	case bytes.HasPrefix(data, []byte(vocabularyMarker)):
		return 2
	case bytes.HasPrefix(data, []byte(vocabularyMarkerV1)):
		return 1
	default:
		return 0
	}
}

func vocabularyVersion(data []byte, path string) (int, error) {
	if version := vocabularyMarkerVersion(data); version > 0 {
		return version, nil
	}
	marker, err := os.ReadFile(path + ".vocabulary")
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return vocabularyMarkerVersion(marker), nil
}

// ReplacementKey is shared by validation and the one-time stored-config upgrade.
func ReplacementKey(key string) string {
	switch key {
	case "remuda":
		return "pool"
	case "landing_mode":
		return "merge_mode"
	case "auto_unsaddle":
		return "auto_teardown"
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

func preserveVocabularyBackup(path string, original []byte) error {
	version, err := vocabularyVersion(original, path)
	if err != nil {
		return err
	}
	if version > 0 {
		return nil
	}
	var values map[string]any
	if _, err := toml.Decode(string(original), &values); err != nil {
		return err
	}
	if !containsLegacyVocabulary(values, "") {
		return nil
	}
	return writeFileIfMissing(path+".v014", original, filePerm(path))
}

func containsLegacyVocabulary(values map[string]any, prefix string) bool {
	for key, value := range values {
		full := key
		if prefix != "" {
			full = prefix + "." + key
		}
		if ReplacementKey(full) != "" {
			return true
		}
		if key == "auto_teardown" && value == "landed" {
			return true
		}
		if text, ok := value.(string); ok && migrateVocabularyString(full, text) != text {
			return true
		}
		if table, ok := value.(map[string]any); ok && containsLegacyVocabulary(table, full) {
			return true
		}
		if full == "dispatch" {
			if rules, ok := tableEntries(value); ok {
				for _, rule := range rules {
					if legacyTaskType(rule["type"]) != "" {
						return true
					}
				}
			}
		}
	}
	return false
}

func writeFileIfMissing(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

// migrateVocabularyFile upgrades stored config once per vocabulary version,
// before it is validated. The v2 migration renames review local to quick; later
// explicit local values are rejected rather than treated as an alias.
func migrateVocabularyFile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	version, err := vocabularyVersion(data, path)
	if err != nil || version >= 2 {
		return err
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
	if err != nil {
		return err
	}
	version, err = vocabularyVersion(data, path)
	if err != nil || version >= 2 {
		return err
	}
	values := map[string]any{}
	if _, err := toml.Decode(string(data), &values); err != nil {
		return &InvalidError{File: path, Key: tomlErrorKey(err), Cause: err}
	}
	originalValues := clone(values).(map[string]any)
	var changed bool
	if version == 1 {
		changed = migrateReviewPolicies(values, "")
	} else {
		changed, err = migrateVocabularyMap(values, "")
	}
	if err != nil {
		return &InvalidError{File: path, Key: "config", Cause: err}
	}
	// Check the rewritten schema before recording completion. Allow Project
	// shorthand here; Load enforces the actual file's global/Project scope.
	validationValues := clone(values).(map[string]any)
	removeLegacyGateSettings(path, validationValues)
	if err := ValidateSchemaMap(path, validationValues, true); err != nil {
		return err
	}
	inlineVersion := vocabularyMarkerVersion(data)
	if !changed {
		if inlineVersion == 1 {
			updated := append([]byte(vocabularyMarker), data[len(vocabularyMarkerV1):]...)
			return atomicfile.Write(path, updated, filePerm(path))
		}
		return atomicfile.Write(path+".vocabulary", []byte(vocabularyMarker), 0o600)
	}
	updatedText, err := rewriteVocabularyText(string(data), values)
	if err != nil {
		return &InvalidError{File: path, Key: "config", Cause: err}
	}
	if inlineVersion == 1 {
		updatedText = strings.TrimPrefix(updatedText, vocabularyMarkerV1)
	}
	updated := append([]byte(vocabularyMarker), []byte(updatedText)...)
	if containsReviewLocal(originalValues, "") {
		if err := writeFileIfMissing(path+".v015", data, filePerm(path)); err != nil {
			return err
		}
	}
	if version == 0 && containsLegacyVocabulary(originalValues, "") {
		if err := writeFileIfMissing(path+".v014", data, filePerm(path)); err != nil {
			return err
		}
	}
	return atomicfile.Write(path, updated, filePerm(path))
}

func containsReviewLocal(values map[string]any, prefix string) bool {
	for key, value := range values {
		full := key
		if prefix != "" {
			full = prefix + "." + key
		}
		if text, ok := value.(string); ok && isReviewPolicyPath(full) && text == "local" {
			return true
		}
		if table, ok := value.(map[string]any); ok && containsReviewLocal(table, full) {
			return true
		}
	}
	return false
}

func isReviewPolicyPath(path string) bool {
	if path == "review" || path == "defaults.review" {
		return true
	}
	return strings.HasPrefix(path, "repositories.") && strings.HasSuffix(path, ".review") && strings.Count(path, ".") == 2
}

func migrateReviewPolicies(values map[string]any, prefix string) bool {
	changed := false
	for key, value := range values {
		full := key
		if prefix != "" {
			full = prefix + "." + key
		}
		if text, ok := value.(string); ok && isReviewPolicyPath(full) && text == "local" {
			values[key], value, changed = "quick", "quick", true
		}
		if table, ok := value.(map[string]any); ok && migrateReviewPolicies(table, full) {
			changed = true
		}
	}
	return changed
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
		if text, ok := value.(string); ok && isReviewPolicyPath(full) && text == "local" {
			values[key], value, changed = "quick", "quick", true
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
