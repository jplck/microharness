package main

import (
	"fmt"
	"os"

	"github.com/jplck/micro/toolplugin"
	"github.com/jplck/micro/toolplugin/builtin"
)

func main() {
	if err := toolplugin.Serve(builtin.Tools()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
