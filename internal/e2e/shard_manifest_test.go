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
	"strings"
	"testing"
)

const shardManifestPath = "../../scripts/e2e-shards.txt"

var shardIDs = []string{"1", "2", "3", "4"}

// TestShardManifestCoversEveryE2ETest runs without the e2e tag so a plain
// `go test ./...` catches E2E tests missing from scripts/e2e-shards.txt, the
// manifest scripts/run-e2e-shard.sh reads.
func TestShardManifestCoversEveryE2ETest(t *testing.T) {
	assigned, counts := readManifest(t, shardManifestPath)
	discovered := discoverE2ETests(t)

	var problems []string
	for _, name := range sortedKeys(discovered) {
		if _, ok := assigned[name]; ok {
			continue
		}
		shard := lightestShard(counts)
		problems = append(problems, fmt.Sprintf(
			"E2E test %s (%s) is not assigned to a shard; add this line to scripts/e2e-shards.txt (shard %s currently has the fewest tests):\n\t%s %s",
			name, discovered[name], shard, shard, name))
		counts[shard]++
	}
	for _, name := range sortedKeys(assigned) {
		if _, ok := discovered[name]; !ok {
			problems = append(problems, fmt.Sprintf(
				"scripts/e2e-shards.txt line %d assigns %s, which is not a top-level test in internal/e2e; remove it", assigned[name].line, name))
		}
	}
	if len(problems) > 0 {
		t.Fatalf("E2E shard manifest is out of date:\n%s", strings.Join(problems, "\n"))
	}
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
	return strings.HasPrefix(name, "Test") && name != "TestMain" && name != "TestShardManifestCoversEveryE2ETest"
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
