package api

import (
	"context"
	"fmt"
)

func memoryTools() []Tool {
	kind := Parameter{Name: "kind", Type: String, Description: "Memory kind: fact, episode, or procedure", Required: true}
	return []Tool{
		contextTool(Tool{
			Name: "search_memory", Description: "Search the memory for specific information",
			Parameters: []Parameter{{Name: "query", Type: String, Description: "The query to search for in the memory", Required: true}},
		}, func(ctx context.Context, binding ToolContext, arguments struct{ Query string }) (string, error) {
			return fmt.Sprintf("Search results for query '%s': %v", arguments.Query, binding.Environment.MemoryStore.Search(arguments.Query)), nil
		}),
		contextTool(Tool{
			Name: "write_memory", Description: "Write specific information to the memory",
			Parameters: []Parameter{kind, {Name: "content", Type: String, Description: "The content to write to the memory", Required: true}},
		}, func(ctx context.Context, binding ToolContext, arguments struct{ Kind, Content string }) (string, error) {
			if err := binding.Environment.MemoryStore.Add(ctx, Memory{Kind: arguments.Kind, Content: arguments.Content}); err != nil {
				return "", err
			}
			return fmt.Sprintf("Written to memory: '%s'", arguments.Content), nil
		}),
		contextTool(Tool{
			Name: "update_memory", Description: "Update specific information in the memory",
			Parameters: []Parameter{
				{Name: "id", Type: String, Description: "The ID of the memory entry to update", Required: true},
				kind,
				{Name: "content", Type: String, Description: "The content to update in the memory", Required: true},
			},
		}, func(ctx context.Context, binding ToolContext, arguments struct{ ID, Kind, Content string }) (string, error) {
			if _, err := binding.Environment.MemoryStore.Update(ctx, Memory{ID: arguments.ID, Kind: arguments.Kind, Content: arguments.Content}); err != nil {
				return "", err
			}
			return fmt.Sprintf("Updated memory: '%s'", arguments.Content), nil
		}),
	}
}
