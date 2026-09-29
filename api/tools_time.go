package api

import (
	"context"
	"fmt"
	"time"
)

func timeTool() Tool {
	return Tool{
		Name: "get_time", Description: "Get the current time in a specific location",
		Parameters: []Parameter{{Name: "location", Type: String, Description: "The location to get the current time for", Required: true}},
		Execute: JSONHandler(func(ctx context.Context, arguments struct {
			Location string `json:"location"`
		}) (string, error) { return fmt.Sprintf("Current time in %s: %s", arguments.Location, time.Now().Format(time.RFC3339)), nil }),
	}
}
