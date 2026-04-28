package main

import (
	"context"
	"fmt"
	"os"

	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	"github.com/takumanakagame/ccmanage/internal/cli"
)

func main() {
	if err := cli.Root(buildinfo.Version).ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
