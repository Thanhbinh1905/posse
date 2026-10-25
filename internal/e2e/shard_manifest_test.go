//go:build !e2e

package e2e

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const e2eDurationsPath = "../../scripts/e2e-durations.txt"

// newTestDurationEstimate is the duration suggested for a test with no
// recorded CI timing yet; refresh it with scripts/refresh-e2e-durations.sh.
const newTestDurationEstimate = 60

var shardIDs = []string{"1", "2", "3", "4"}

// TestE2EDurationsCoverEveryE2ETest runs without the e2e tag so a plain
// `go test ./...` catches E2E tests missing from scripts/e2e-durations.txt,
// which scripts/run-e2e-shard.sh balances shards by.
func TestE2EDurationsCoverEveryE2ETest(t *testing.T) {
	recorded := readDurations(t, e2eDurationsPath)
	discovered := discoverE2ETests(t)

	var problems []string
	for _, name := range sortedKeys(discovered) {
		if _, ok := recorded[name]; ok {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"E2E test %s (%s) has no recorded duration; add this line to scripts/e2e-durations.txt and refresh it after CI runs with scripts/refresh-e2e-durations.sh <run-id>:\n\t%d %s",
			name, discovered[name], newTestDurationEstimate, name))
	}
	for _, name := range sortedKeys(recorded) {
		if _, ok := discovered[name]; !ok {
			problems = append(problems, fmt.Sprintf(
				"scripts/e2e-durations.txt line %d records %s, which is not a top-level test in internal/e2e; remove it", recorded[name], name))
		}
	}
	if len(problems) > 0 {
		t.Fatalf("E2E durations are out of date:\n%s", strings.Join(problems, "\n"))
	}
}

// readDurations maps each recorded test to its line number.
func readDurations(t *testing.T, path string) map[string]int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open E2E durations: %v", err)
	}
	defer f.Close()

	recorded := map[string]int{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			t.Fatalf("%s line %d: expected \"<seconds> <name>\"", path, n)
		}
		if seconds, err := strconv.Atoi(fields[0]); err != nil || seconds < 1 {
			t.Fatalf("%s line %d: invalid duration %s, expected whole seconds of at least 1", path, n, fields[0])
		}
		if prev, dup := recorded[fields[1]]; dup {
			t.Fatalf("%s line %d: %s is already recorded on line %d", path, n, fields[1], prev)
		}
		recorded[fields[1]] = n
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read E2E durations: %v", err)
	}
	return recorded
}

type assignment struct {
	shard string
	line  int
}

func readManifest(t *testing.T, path string) (map[string]assignment, map[string]int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open shard manifest: %v", err)
	}
	defer f.Close()

	assigned := map[string]assignment{}
	counts := map[string]int{}
	for _, id := range shardIDs {
		counts[id] = 0
	}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			t.Fatalf("%s line %d: expected \"<shard> <name>\"", path, n)
		}
		shard, name := fields[0], fields[1]
		if _, ok := counts[shard]; !ok {
			t.Fatalf("%s line %d: invalid shard %s, expected one of %v", path, n, shard, shardIDs)
		}
		if prev, dup := assigned[name]; dup {
			t.Fatalf("%s line %d: %s is already assigned on line %d", path, n, name, prev.line)
		}
		assigned[name] = assignment{shard: shard, line: n}
		counts[shard]++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read shard manifest: %v", err)
	}
	return assigned, counts
}

// discoverE2ETests lists top-level Test functions across every platform's
// files, mirroring `go test -list '^Test'` except for TestMain.
func discoverE2ETests(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	tests := map[string]string{}
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if !runsInE2E(t, parsed) {
			continue
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isTestName(fn.Name.Name) {
				continue
			}
			if _, seen := tests[fn.Name.Name]; !seen {
				tests[fn.Name.Name] = file
			}
		}
	}
	return tests
}

// runsInE2E reports whether the shard script's `-tags=e2e` build includes the
// file on some platform. Files needing other tags (such as perf) are skipped.
func runsInE2E(t *testing.T, file *ast.File) bool {
	t.Helper()
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				t.Fatalf("parse build constraint %q: %v", c.Text, err)
			}
			for _, goos := range []string{"linux", "darwin"} {
				if expr.Eval(func(tag string) bool { return tag == "e2e" || tag == goos }) {
					return true
				}
			}
			return false
		}
	}
	return true
}

func isTestName(name string) bool {
	return strings.HasPrefix(name, "Test") && name != "TestMain" && name != "TestE2EDurationsCoverEveryE2ETest"
}

func lightestShard(counts map[string]int) string {
	best := shardIDs[0]
	for _, id := range shardIDs[1:] {
		if counts[id] < counts[best] {
			best = id
		}
	}
	return best
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

const (
	raceShardManifestPath = "../../scripts/race-shards.txt"
	raceAppPackage        = "github.com/thanhbinh1905/posse/internal/app"
)

// TestRaceShardManifestCoversEveryPackage guards scripts/race-shards.txt, read
// by scripts/run-race-shard.sh. The app package is sharded by test name and
// must not appear in the manifest.
func TestRaceShardManifestCoversEveryPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "../../...").Output()
	if err != nil {
		t.Fatalf("go list ./...: %v", err)
	}
	discovered := map[string]bool{}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg != raceAppPackage {
			discovered[pkg] = true
		}
	}

	assigned, counts := readManifest(t, raceShardManifestPath)
	var problems []string
	for _, pkg := range sortedKeys(discovered) {
		if _, ok := assigned[pkg]; ok {
			continue
		}
		shard := lightestShard(counts)
		problems = append(problems, fmt.Sprintf(
			"package %s is not assigned to a race shard; add this line to scripts/race-shards.txt (shard %s currently has the fewest packages):\n\t%s %s",
			pkg, shard, shard, pkg))
		counts[shard]++
	}
	for _, pkg := range sortedKeys(assigned) {
		if !discovered[pkg] {
			problems = append(problems, fmt.Sprintf(
				"scripts/race-shards.txt line %d assigns %s, which is not a non-app package; remove it", assigned[pkg].line, pkg))
		}
	}
	if len(problems) > 0 {
		t.Fatalf("race shard manifest is out of date:\n%s", strings.Join(problems, "\n"))
	}
}
