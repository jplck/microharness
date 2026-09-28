package api

import (
	"context"
	"fmt"
	"time"
)

func MemoryTools(ctx context.Context, store *MemoryStore) []Tool {
	return []Tool{
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
				matches := store.Search(arguments.Query)
				return fmt.Sprintf("Search results for query '%s': %v", arguments.Query, matches), nil
			}),
		},
		{
			Name:        "write_memory",
			Description: "Write specific information to the memory",
			Parameters: []Parameter{
				{
					Name:        "kind",
					Type:        String,
					Description: "Memory kind: fact, episode, or procedure",
					Required:    true,
				},
				{
					Name:        "content",
					Type:        String,
					Description: "The content to write to the memory",
					Required:    true,
				},
			},
			Execute: JSONHandler(func(ctx context.Context, arguments struct {
				Kind    string `json:"kind"`
				Content string `json:"content"`
			}) (string, error) {

				switch arguments.Kind {
				case "fact", "episode", "procedure":
				default:
					return "", fmt.Errorf("kind must be fact, episode, or procedure")
				}

				if err := store.Add(ctx, Memory{
					Kind:      arguments.Kind,
					Content:   arguments.Content,
					UpdatedAt: time.Now(),
				}); err != nil {
					return "", err
				}
				return fmt.Sprintf("Written to memory: '%s' at %s", arguments.Content, time.Now().Format(time.RFC3339)), nil
			}),
		},
		{
			Name:        "update_memory",
			Description: "Update specific information in the memory",
			Parameters: []Parameter{
				{
					Name:        "id",
					Type:        String,
					Description: "The ID of the memory entry to update",
					Required:    true,
				},
				{
					Name:        "kind",
					Type:        String,
					Description: "Memory kind: fact, episode, or procedure",
					Required:    true,
				},
				{
					Name:        "content",
					Type:        String,
					Description: "The content to update in the memory",
					Required:    true,
				},
			},
			Execute: JSONHandler(func(ctx context.Context, arguments struct {
				Kind    string `json:"kind"`
				Content string `json:"content"`
				ID      string `json:"id"`
			}) (string, error) {

				switch arguments.Kind {
				case "fact", "episode", "procedure":
				default:
					return "", fmt.Errorf("kind must be fact, episode, or procedure")
				}

				if _, err := store.Update(ctx, Memory{
					Kind:    arguments.Kind,
					Content: arguments.Content,
					ID:      arguments.ID,
				}); err != nil {
					return "", err
				}
				return fmt.Sprintf("Updated memory: '%s' at %s", arguments.Content, time.Now().Format(time.RFC3339)), nil
			}),
		},
	}
}
