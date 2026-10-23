package app

import "testing"

// goSourceNamingHerdr is Posse source text that names the herdr package and
// the herdr program as data, the way an inline edit of internal/app carries it.
const goSourceNamingHerdr = `package app

import (
	"os/exec"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func snapshot(adapter herdr.Adapter) herdr.Snapshot {
	_ = exec.Command("herdr", "pane", "list")
	return herdr.Snapshot{}
}
`

func TestGuardAllowsInterpreterEditsOfSourceNamingHerdr(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{
		"python3 - <<'EOF'\nfrom pathlib import Path\np = Path('internal/app/teardown_panes.go')\ns = p.read_text()\ns = s.replace('herdr.Snapshot{}', 'herdr.Snapshot{Panes: nil}')\np.write_text(s)\nEOF",
		"python3 - <<'EOF'\nopen('internal/app/adapter.go', 'w').write('''" + goSourceNamingHerdr + "''')\nEOF",
		"python3 -I - <<'EOF'\np = 'internal/app/guard.go'\ns = open(p).read()\ns = s.replace('case name == \"herdr\":', 'case name == \"herdr\" || name == \"herdr.exe\":')\nopen(p, 'w').write(s)\nEOF",
		`perl -0pi -e 's/herdr\.Snapshot\{\}/herdr.Snapshot{Panes: nil}/' internal/app/teardown_panes.go`,
		"perl -0pi -e 's/\"github.com\\/thanhbinh1905\\/posse\\/internal\\/herdr\"/\"herdr\"/' internal/app/service.go",
		`ruby -i -pe 'gsub(/herdr\.Pane/, "herdr.PaneInfo")' internal/app/pool.go`,
		`node -e 'const fs = require("fs"); const p = "internal/app/pool.go"; fs.writeFileSync(p, fs.readFileSync(p, "utf8").replace("herdr.Pane", "herdr.PaneInfo"))'`,
		`awk '{ gsub(/herdr\.Pane/, "herdr.PaneInfo"); print }' internal/app/pool.go`,
		"printf input | python3 -c \"print('herdr')\"",
	} {
		if refused, reason := guardCommand(command, scope); refused {
			t.Errorf("guard refused an edit that cannot reach Herdr %q: %#v", command, reason)
		}
	}
}

func TestGuardRefusesInterpreterCodeThatRunsHerdrOrOpensItsSocket(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{
		// An edit that also runs the herdr program.
		"python3 - <<'EOF'\nimport subprocess\nfrom pathlib import Path\nPath('internal/app/pool.go').write_text('package app')\nsubprocess.run(['herdr', 'workspace', 'close', 'w1'])\nEOF",
		"python3 - <<'EOF'\nimport os\nos.system('cd /tmp && herdr pane close w1:p1')\nEOF",
		"python3 - <<'EOF'\nimport os\nos.execvp('/home/u/.local/bin/herdr', ['herdr', 'pane', 'close', 'w1:p1'])\nEOF",
		"python3 - <<'EOF'\nimport subprocess\nsubprocess.run('~/.local/bin/herdr pane close w1:p1', shell=True)\nEOF",
		// Socket access needs no herdr program.
		"python3 - <<'EOF'\nimport os, socket\nsocket.socket(socket.AF_UNIX).connect(os.environ['HERDR_SOCKET_PATH'])\nEOF",
		"python3 - <<'EOF'\nimport socket\nsocket.socket(socket.AF_UNIX).connect('/home/u/.config/herdr/herdr.sock')\nEOF",
		`perl -e 'print ` + "`herdr pane list`" + `'`,
		`perl -e 'exec "herdr", "pane", "close", "w1:p1"'`,
		`ruby -e 'IO.popen(["herdr", "pane", "close", "w1:p1"]).read'`,
		`node -e 'require("child_process").spawnSync("herdr", ["pane", "close", "w1:p1"])'`,
		`awk 'BEGIN { system("herdr pane close w1:p1") }'`,
		`awk 'BEGIN { while (("herdr pane list" | getline line) > 0) print line }'`,
		"expect - <<'EOF'\nspawn herdr pane list\nEOF",
		// Interactive and startup-override modes can run code the command does not show.
		"PYTHONSTARTUP=probe python3 - <<'EOF'\nprint('herdr')\nEOF",
		"python3 -i - <<'EOF'\nprint('herdr')\nEOF",
	} {
		if refused, reason := guardCommand(command, scope); !refused || reason.why == "" {
			t.Errorf("guard allowed interpreter code that can reach Herdr %q: %#v", command, reason)
		}
	}
}

func TestGuardAllowsInterpreterHerdrAccessToAnIsolatedServer(t *testing.T) {
	scope, _ := guardFixture(t)
	command := "HERDR_SOCKET_PATH=/tmp/posse-e2e-x/herdr.sock python3 - <<'EOF'\nimport subprocess\nsubprocess.run(['herdr', 'pane', 'list'])\nEOF"
	if refused, reason := guardCommand(command, scope); refused {
		t.Fatalf("guard refused Herdr access to an isolated server: %#v", reason)
	}
}
