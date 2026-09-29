package main

import (
	"fmt"
	"os"

	"github.com/jplck/micro/toolplugin"
)

func main() {
	tool := toolplugin.Tool{
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
	if err := toolplugin.Serve([]toolplugin.Tool{tool}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
