package setup

import (
	"strings"
	"testing"
)

func TestLifecycleSkillsAreEmbeddedAndUserInvoked(t *testing.T) {
	for _, name := range []string{"posse-down", "posse-update"} {
		data, err := Skill(name)
		if err != nil {
			t.Fatalf("Skill(%q): %v", name, err)
		}
		content := string(data)
		for _, required := range []string{"name: " + name, "disable-model-invocation: true"} {
			if !strings.Contains(content, required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		if !strings.Contains(content, "Never run the command yourself") && !strings.Contains(content, "never run them yourself") {
			t.Errorf("%s must reserve command execution for the User", name)
		}
		if name == "posse-down" && (!strings.Contains(content, "`posse down`") || strings.Contains(content, "run `posse down` yourself")) {
			t.Errorf("%s must name the exact command and reserve execution for the User", name)
		}
		if name == "posse-update" && (!strings.Contains(content, "`posse update`") || strings.Contains(content, "run `posse update` yourself")) {
			t.Errorf("%s must name the exact command and reserve execution for the User", name)
		}
	}
}
