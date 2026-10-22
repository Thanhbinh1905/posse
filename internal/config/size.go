package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var byteSizeUnits = []struct {
	suffix     string
	multiplier int64
}{
	{"TiB", 1 << 40},
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

// ParseByteSize parses a positive whole number of bytes with an optional
// binary unit suffix, such as "8GiB" or "512MiB".
func ParseByteSize(text string) (int64, error) {
	trimmed := strings.TrimSpace(text)
	multiplier := int64(1)
	for _, unit := range byteSizeUnits {
		if strings.HasSuffix(trimmed, unit.suffix) {
			trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, unit.suffix))
			multiplier = unit.multiplier
			break
		}
	}
	number, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || number <= 0 || number > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("must be a positive size in bytes or with a KiB, MiB, GiB or TiB suffix")
	}
	return number * multiplier, nil
}
