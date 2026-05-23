package main

import (
	"context"
	"fmt"
	"os"

	"github.com/yusing/git-agent/internal/cli"
)

func main() {
	ctx := context.Background()
	app := cli.New()
	if err := app.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
