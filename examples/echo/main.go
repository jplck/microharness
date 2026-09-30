package main

import (
	"fmt"
	"os"

	"github.com/jplck/micro/toolplugin"
)

func echoTool() toolplugin.Tool {
	return toolplugin.Tool{
		Definition: toolplugin.Definition{
			Name: "echo", Description: "Return the supplied text",
			Parameters: []toolplugin.Parameter{{Name: "input", Type: toolplugin.String, Description: "Text to return", Required: true}},
		},
		Call: toolplugin.Handler(func(arguments struct {
			Input string `json:"input"`
		}) (string, error) {
			return arguments.Input, nil
		}),
	}
}

func main() {
	if err := toolplugin.Serve([]toolplugin.Tool{echoTool()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
