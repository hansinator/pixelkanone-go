package main

import (
	"os"

	"github.com/hansinator/pixelkanone-go/pkg/cli"
)

func main() {
	os.Exit(cli.RunCommandLineTool())
}
