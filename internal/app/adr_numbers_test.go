package app

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// legacyDuplicateADRNumbers are numbers shared before this check existed.
// Renumbering them would break links, so they stay; no new entry is allowed.
var legacyDuplicateADRNumbers = map[string]int{"0013": 2, "0020": 2}

var adrFileName = regexp.MustCompile(`^(\d{4})-.+\.md$`)

func TestADRNumbersAreUnique(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "adr")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	byNumber := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := adrFileName.FindStringSubmatch(entry.Name())
		if m == nil {
			t.Errorf("docs/adr/%s does not match NNNN-title.md", entry.Name())
			continue
		}
		byNumber[m[1]] = append(byNumber[m[1]], entry.Name())
	}
	numbers := make([]string, 0, len(byNumber))
	for number := range byNumber {
		numbers = append(numbers, number)
	}
	sort.Strings(numbers)
	for _, number := range numbers {
		files := byNumber[number]
		allowed := legacyDuplicateADRNumbers[number]
		if allowed == 0 {
			allowed = 1
		}
		if len(files) > allowed {
			t.Errorf("ADR number %s is shared by %d files: %s", number, len(files), strings.Join(files, ", "))
		}
	}
}
