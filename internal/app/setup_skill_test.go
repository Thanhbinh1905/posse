package app

import (
	"regexp"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

// TestPosseSetupSkillNamesOnlySchemaKeys keeps the skill's config key names in
// step with `posse config schema`, including which keys are User-only.
func TestPosseSetupSkillNamesOnlySchemaKeys(t *testing.T) {
	skill, err := setupassets.Skill("posse-setup")
	if err != nil {
		t.Fatal(err)
	}
	keyPattern := regexp.MustCompile(`^(identity|lead|defaults|remuda|kinds|profiles|dispatch|autonomy)(\.[a-z_<>*]+)*$`)
	// A bare field name such as `gate` is not a key; config set refuses it.
	fieldNames := map[string]bool{}
	for _, spec := range config.Schema() {
		parts := strings.Split(spec.Key, ".")
		if len(parts) > 1 && parts[0] != "dispatch" {
			fieldNames[parts[len(parts)-1]] = true
		}
	}
	checked := map[string]bool{}
	for _, span := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(string(skill), -1) {
		fields := strings.Fields(strings.TrimPrefix(span[1], "! "))
		if len(fields) >= 4 && fields[0] == "posse" && fields[1] == "config" && (fields[2] == "set" || fields[2] == "unset") {
			fields = fields[3:]
		}
		if len(fields) == 1 && fieldNames[fields[0]] {
			t.Errorf("skill names bare field %q instead of its full schema key", fields[0])
			continue
		}
		if len(fields) == 0 || !keyPattern.MatchString(fields[0]) {
			continue
		}
		key := fields[0]
		if strings.HasSuffix(key, ".*") {
			if !config.IsUserOnly(strings.TrimSuffix(key, "*") + "review") {
				t.Errorf("skill groups %s as User-only, but the schema disagrees", key)
			}
			continue
		}
		concrete := strings.NewReplacer("<kind>", "claude", "<profile>", "deep").Replace(key)
		if _, found := config.Lookup(concrete); !found {
			t.Errorf("skill names %q, which is not a schema key", key)
		}
		checked[key] = true
	}
	for _, key := range []string{"defaults.gate", "remuda.setup", "autonomy.review", "autonomy.land", "autonomy.yolo"} {
		if !checked[key] {
			t.Errorf("skill no longer names User-only key %s", key)
		}
	}
	for _, line := range strings.Split(string(skill), "\n") {
		if strings.Contains(line, "posse config set defaults.gate") || strings.Contains(line, "posse config set autonomy.") {
			if !strings.Contains(line, "`! posse config set") {
				t.Errorf("skill tells the agent to run a User-only write itself: %s", line)
			}
		}
	}
}
