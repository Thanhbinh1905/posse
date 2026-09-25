package main

import (
	"fmt"
	"os"

	"github.com/thanhbinh1905/posse/internal/app"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

var version = "dev"

func main() {
	service := app.New("", herdr.New())
	store.MigrationGate = service.MigrationGate
	if len(os.Args) > 1 && os.Args[1] == "_ingest" {
		os.Exit(service.RunIngest(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "_guard" {
		os.Exit(service.RunGuard(os.Stdin, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "_finalize-lead" {
		os.Exit(service.RunLeadFinalizer(os.Args[2:]))
	}
	service.Version = version
	cli := service.CLI()
	cli.Name = "posse"
	cli.Version = version
	code := cli.Run(os.Args[1:])
	if code == 0 {
		if err := service.ExecPendingLead(); err != nil {
			fmt.Fprintln(os.Stderr, "posse: could not start Lead in this pane:", err)
			os.Exit(1)
		}
	}
	os.Exit(code)
}
