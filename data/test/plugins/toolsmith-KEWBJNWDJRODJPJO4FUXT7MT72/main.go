package main

import (
	"fmt"
	"os"
	"time"

	"github.com/jplck/micro/toolplugin"
)

func main() {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load America/Los_Angeles location: %v\n", err)
		os.Exit(1)
	}

	err = toolplugin.Serve([]toolplugin.Tool{
		{
			Definition: toolplugin.Definition{
				Name:        "seattle_time",
				Description: "Converts a current UTC time string (RFC 3339, e.g. 2025-01-15T14:30:00Z) to the same instant expressed in Seattle local time (America/Los_Angeles), automatically handling PDT/PST, formatted as a readable string.",
				Parameters: []toolplugin.Parameter{
					{Name: "utc_time", Type: toolplugin.String, Description: "Current UTC time in RFC 3339 format, e.g. 2025-01-15T14:30:00Z", Required: true},
				},
			},
			Call: toolplugin.Handler(func(args struct {
				UtcTime string `json:"utc_time"`
			}) (string, error) {
				t, err := time.Parse(time.RFC3339, args.UtcTime)
				if err != nil {
					return "", fmt.Errorf("invalid UTC time %q: %w", args.UtcTime, err)
				}
				return t.In(loc).Format("MST, Jan 02 15:04:05 2006"), nil
			}),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "seattle_time serve error: %v\n", err)
		os.Exit(1)
	}
}
