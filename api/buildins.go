package api

import (
	"context"
	"fmt"
	"time"
)

func BuiltinTools() ToolRegistry {
	return ToolRegistry{
		"get_time": {
			Name:        "get_time",
			Description: "Get the current time in a specific location",
			Parameters: []Parameter{{
				Name: "location", Type: String,
				Description: "The location to get the current time for", Required: true,
			}},
			Execute: JSONHandler(func(ctx context.Context, arguments struct {
				Location string `json:"location"`
			}) (string, error) {
				return fmt.Sprintf("Current time in %s: %s", arguments.Location, time.Now().Format(time.RFC3339)), nil
			}),
		},
	}
}

func MemoryTools(ctx context.Context, store *MemoryStore) []Tool {
	kind := Parameter{Name: "kind", Type: String, Description: "Memory kind: fact, episode, or procedure", Required: true}
	return []Tool{
		{
			Name: "search_memory", Description: "Search the memory for specific information",
			Parameters: []Parameter{{Name: "query", Type: String, Description: "The query to search for in the memory", Required: true}},
			Execute: JSONHandler(func(ctx context.Context, arguments struct{ Query string }) (string, error) {
				return fmt.Sprintf("Search results for query '%s': %v", arguments.Query, store.Search(arguments.Query)), nil
			}),
		},
		{
			Name: "write_memory", Description: "Write specific information to the memory",
			Parameters: []Parameter{kind, {Name: "content", Type: String, Description: "The content to write to the memory", Required: true}},
			Execute: JSONHandler(func(ctx context.Context, arguments struct{ Kind, Content string }) (string, error) {
				if err := store.Add(ctx, Memory{Kind: arguments.Kind, Content: arguments.Content}); err != nil {
					return "", err
				}
				return fmt.Sprintf("Written to memory: '%s'", arguments.Content), nil
			}),
		},
		{
			Name: "update_memory", Description: "Update specific information in the memory",
			Parameters: []Parameter{
				{Name: "id", Type: String, Description: "The ID of the memory entry to update", Required: true},
				kind,
				{Name: "content", Type: String, Description: "The content to update in the memory", Required: true},
			},
			Execute: JSONHandler(func(ctx context.Context, arguments struct{ ID, Kind, Content string }) (string, error) {
				if _, err := store.Update(ctx, Memory{ID: arguments.ID, Kind: arguments.Kind, Content: arguments.Content}); err != nil {
					return "", err
				}
				return fmt.Sprintf("Updated memory: '%s'", arguments.Content), nil
			}),
		},
	}
}
