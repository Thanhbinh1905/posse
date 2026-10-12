package app

import (
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
)

type flagSpec struct {
	boolean    bool
	repeatable bool
}

type parsedArgs struct {
	Flags       map[string]string
	Repeated    map[string][]string
	Positionals []string
}

func parseArgs(command string, args []string, specs map[string]flagSpec) (parsedArgs, error) {
	result := parsedArgs{Flags: map[string]string{}, Repeated: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			result.Positionals = append(result.Positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			result.Positionals = append(result.Positionals, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		spec, found := specs[name]
		if !found {
			return parsedArgs{}, axi.Usage(fmt.Sprintf("unknown %s flag --%s", command, name))
		}
		if spec.boolean {
			if hasValue {
				return parsedArgs{}, axi.Usage(fmt.Sprintf("--%s does not take a value", name))
			}
			result.Flags[name] = "true"
			continue
		}
		if !hasValue {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return parsedArgs{}, axi.Usage(fmt.Sprintf("--%s requires a value", name))
			}
			i++
			value = args[i]
		}
		result.Flags[name] = value
		if spec.repeatable {
			result.Repeated[name] = append(result.Repeated[name], value)
		}
	}
	return result, nil
}

func (a parsedArgs) Bool(name string) bool { return a.Flags[name] == "true" }
