package main

import (
	"os"

	"github.com/davinci-std/kanvas/cmd"
)

func main() {
	if err := cmd.Root().Execute(); err != nil {
		os.Exit(1)
	}
}
