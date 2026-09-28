package api

import (
	"context"
	"fmt"
	"time"
)

var MemoryTools = []Tool{
	{
		Name:        "search_memory",
		Description: "Search the memory for specific information",
		Parameters: []Parameter{
			{
				Name:        "query",
				Type:        String,
				Description: "The query to search for in the memory",
				Required:    true,
			},
		},
		Execute: JSONHandler(func(ctx context.Context, arguments struct {
			Query string `json:"query"`
		}) (string, error) {
			return fmt.Sprintf("Search results for query '%s': %s", arguments.Query, time.Now().Format(time.RFC3339)), nil
		}),
	},
	{
		Name:        "write_memory",
		Description: "Write specific information to the memory",
		Parameters: []Parameter{
			{
				Name:        "content",
				Type:        String,
				Description: "The content to write to the memory",
				Required:    true,
			},
		},
		Execute: JSONHandler(func(ctx context.Context, arguments struct {
			Content string `json:"content"`
		}) (string, error) {
			return fmt.Sprintf("Written to memory: '%s' at %s", arguments.Content, time.Now().Format(time.RFC3339)), nil
		}),
	},
}
