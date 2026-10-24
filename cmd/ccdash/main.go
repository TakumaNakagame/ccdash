package main

import (
	"context"
	"fmt"
	"os"

	"github.com/TakumaNakagame/ccdash/internal/buildinfo"
	"github.com/TakumaNakagame/ccdash/internal/cli"
)

func main() {
	if err := cli.Root(buildinfo.Version).ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
