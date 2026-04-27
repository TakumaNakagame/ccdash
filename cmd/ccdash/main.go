package main

import (
	"context"
	"fmt"
	"os"

	"github.com/takumanakagame/ccmanage/internal/cli"
)

// Version is injected at build time via -ldflags "-X main.Version=...".
// `dev` is the placeholder for unreleased builds (go build / go install).
var Version = "dev"

func main() {
	if err := cli.Root(Version).ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
