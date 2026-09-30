package main

import (
	"os"
)

func main() {
	pluginName := pluginNameFromExecutable(os.Args[0])
	if err := Execute(os.Args[1:], os.Stdout, os.Stderr, version, pluginName); err != nil {
		os.Exit(commandExitCode(err))
	}
}

var version = "dev"
