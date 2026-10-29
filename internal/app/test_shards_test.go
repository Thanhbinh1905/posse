package app

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Most tests here change the process working directory or environment, so
// they cannot use t.Parallel. A run without a test selection instead splits
// the tests across child processes of this binary. Selecting tests with -run,
// as CI shards do, keeps one process.
const maxAppTestShards = 8

func runAppTestShards() (int, bool) {
	flag.Parse()
	for _, name := range []string{"test.run", "test.list", "test.bench", "test.fuzz", "test.skip", "test.coverprofile", "test.cpuprofile", "test.memprofile", "test.blockprofile", "test.mutexprofile", "test.trace"} {
		if flag.Lookup(name).Value.String() != "" {
			return 0, false
		}
	}
	shardCount := min(runtime.GOMAXPROCS(0), maxAppTestShards)
	if shardCount < 2 {
		return 0, false
	}
	listed, err := exec.Command(os.Args[0], "-test.list=.").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list app tests:", err)
		return 1, true
	}
	var mu sync.Mutex
	var wait sync.WaitGroup
	code := 0
	for _, shard := range appTestShards(strings.Fields(string(listed)), shardCount) {
		wait.Go(func() {
			command := exec.Command(os.Args[0], appTestShardArgs(os.Args[1:], shard)...)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			err := command.Run()
			mu.Lock()
			defer mu.Unlock()
			_, _ = os.Stdout.WriteString(withoutFinalTestStatus(output.String()))
			if err != nil {
				code = 1
			}
		})
	}
	wait.Wait()
	status := "PASS"
	if code != 0 {
		status = "FAIL"
	}
	if flag.Lookup("test.v").Value.String() == "test2json" {
		status = "\x16" + status
	}
	fmt.Println(status)
	return code, true
}

func appTestShards(tests []string, count int) [][]string {
	shards := make([][]string, min(count, len(tests)))
	for i, test := range tests {
		shards[i%len(shards)] = append(shards[i%len(shards)], test)
	}
	return shards
}

// appTestShardArgs drops the go test cache log, which concurrent children
// would corrupt; such a run is then not cached.
func appTestShardArgs(args, tests []string) []string {
	var shardArgs []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-test.testlogfile") {
			shardArgs = append(shardArgs, arg)
		}
	}
	return append(shardArgs, "-test.run=^("+strings.Join(tests, "|")+")$")
}

func withoutFinalTestStatus(output string) string {
	body := strings.TrimSuffix(output, "\n")
	cut := strings.LastIndex(body, "\n") + 1
	switch strings.TrimPrefix(body[cut:], "\x16") {
	case "PASS", "FAIL":
		return body[:cut]
	}
	return output
}

func TestAppTestShardsSplitTestsRoundRobin(t *testing.T) {
	got := appTestShards([]string{"TestA", "TestB", "TestC", "TestD", "TestE"}, 3)
	want := [][]string{{"TestA", "TestD"}, {"TestB", "TestE"}, {"TestC"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shards = %v, want %v", got, want)
	}
	if got := appTestShards([]string{"TestA"}, 4); !reflect.DeepEqual(got, [][]string{{"TestA"}}) {
		t.Fatalf("one test made shards %v", got)
	}
}

func TestAppTestShardArgsSelectOnlyTheShardAndDropTheTestLog(t *testing.T) {
	got := appTestShardArgs([]string{"-test.paniconexit0", "-test.timeout=10m0s", "-test.testlogfile=/tmp/testlog.txt", "-test.v=test2json"}, []string{"TestA", "TestB"})
	want := []string{"-test.paniconexit0", "-test.timeout=10m0s", "-test.v=test2json", "-test.run=^(TestA|TestB)$"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shard args = %q, want %q", got, want)
	}
}

func TestWithoutFinalTestStatusKeepsEverythingElse(t *testing.T) {
	for _, tc := range []struct{ output, want string }{
		{"--- PASS: TestA (0.00s)\nPASS\n", "--- PASS: TestA (0.00s)\n"},
		{"\x16--- FAIL: TestA (0.00s)\n\x16FAIL\n", "\x16--- FAIL: TestA (0.00s)\n"},
		{"panic: test timed out after 1s\n", "panic: test timed out after 1s\n"},
		{"", ""},
	} {
		if got := withoutFinalTestStatus(tc.output); got != tc.want {
			t.Errorf("withoutFinalTestStatus(%q) = %q, want %q", tc.output, got, tc.want)
		}
	}
}
