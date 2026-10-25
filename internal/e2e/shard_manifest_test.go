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

// newRaceUnitDurationEstimate is the duration suggested for a unit test or
// package with no recorded CI timing yet; most take about a second.
const newRaceUnitDurationEstimate = 1

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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

const raceDurationsPath = "../../scripts/race-durations.txt"

// raceUnit is one line of scripts/race-durations.txt: a whole package when
// test is empty, otherwise one top-level test of a split package.
type raceUnit struct {
	pkg, test string
	line      int
}

// TestRaceDurationsCoverEveryTest guards scripts/race-durations.txt, which
// scripts/run-race-shard.sh balances the race-enabled unit test shards by. A
// package is recorded whole, or test by test when it is split across shards.
func TestRaceDurationsCoverEveryTest(t *testing.T) {
	module := goList(t, "-m")[0]
	discovered := map[string]bool{}
	for _, pkg := range goList(t, "../../...") {
		discovered[strings.TrimPrefix(pkg, module+"/")] = true
	}

	whole := map[string]int{}
	split := map[string]map[string]int{}
	for _, unit := range readRaceDurations(t, raceDurationsPath) {
		if unit.test == "" {
			whole[unit.pkg] = unit.line
			continue
		}
		if split[unit.pkg] == nil {
			split[unit.pkg] = map[string]int{}
		}
		split[unit.pkg][unit.test] = unit.line
	}

	var problems []string
	for _, pkg := range sortedKeys(discovered) {
		_, isWhole := whole[pkg]
		_, isSplit := split[pkg]
		if !isWhole && !isSplit {
			problems = append(problems, fmt.Sprintf(
				"package %s has no recorded duration; add this line to scripts/race-durations.txt and refresh it after CI runs with scripts/refresh-race-durations.sh <run-id>:\n\t%d %s",
				pkg, newRaceUnitDurationEstimate, pkg))
		}
	}
	for _, pkg := range sortedKeys(whole) {
		if !discovered[pkg] {
			problems = append(problems, fmt.Sprintf(
				"scripts/race-durations.txt line %d records %s, which is not a package; remove it", whole[pkg], pkg))
		}
		if _, ok := split[pkg]; ok {
			problems = append(problems, fmt.Sprintf(
				"scripts/race-durations.txt records %s both whole (line %d) and per test; keep one form", pkg, whole[pkg]))
		}
	}
	for _, pkg := range sortedKeys(split) {
		if !discovered[pkg] {
			problems = append(problems, fmt.Sprintf(
				"scripts/race-durations.txt records tests of %s, which is not a package; remove them", pkg))
			continue
		}
		tests := discoverPackageTests(t, module+"/"+pkg)
		for _, name := range sortedKeys(tests) {
			if _, ok := split[pkg][name]; !ok {
				problems = append(problems, fmt.Sprintf(
					"test %s (%s) of %s has no recorded duration; add this line to scripts/race-durations.txt and refresh it after CI runs with scripts/refresh-race-durations.sh <run-id>:\n\t%d %s %s",
					name, tests[name], pkg, newRaceUnitDurationEstimate, pkg, name))
			}
		}
		for _, name := range sortedKeys(split[pkg]) {
			if _, ok := tests[name]; !ok {
				problems = append(problems, fmt.Sprintf(
					"scripts/race-durations.txt line %d records %s of %s, which is not a top-level test; remove it", split[pkg][name], name, pkg))
			}
		}
	}
	if len(problems) > 0 {
		t.Fatalf("race durations are out of date:\n%s", strings.Join(problems, "\n"))
	}
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}

// discoverPackageTests lists the top-level Test functions that `go test`
// builds for pkg on this platform, mapped to their file.
func discoverPackageTests(t *testing.T, pkg string) map[string]string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f",
		"{{.Dir}}{{range .TestGoFiles}}\n{{.}}{{end}}{{range .XTestGoFiles}}\n{{.}}{{end}}", pkg).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	dir := lines[0]
	fset := token.NewFileSet()
	tests := map[string]string{}
	for _, file := range lines[1:] {
		parsed, err := parser.ParseFile(fset, filepath.Join(dir, file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			if _, seen := tests[fn.Name.Name]; !seen {
				tests[fn.Name.Name] = file
			}
		}
	}
	return tests
}

func readRaceDurations(t *testing.T, path string) []raceUnit {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open race durations: %v", err)
	}
	defer f.Close()

	var units []raceUnit
	seen := map[string]int{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 && len(fields) != 3 {
			t.Fatalf("%s line %d: expected \"<seconds> <package> [<test>]\"", path, n)
		}
		if seconds, err := strconv.Atoi(fields[0]); err != nil || seconds < 1 {
			t.Fatalf("%s line %d: invalid duration %s, expected whole seconds of at least 1", path, n, fields[0])
		}
		unit := raceUnit{pkg: fields[1], line: n}
		if len(fields) == 3 {
			unit.test = fields[2]
		}
		key := unit.pkg + " " + unit.test
		if prev, dup := seen[key]; dup {
			t.Fatalf("%s line %d: %s is already recorded on line %d", path, n, strings.TrimSpace(key), prev)
		}
		seen[key] = n
		units = append(units, unit)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read race durations: %v", err)
	}
	return units
}
