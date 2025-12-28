package main

import (
	"encoding/json"
	"fmt"
)

// User-defined custom tools
// Add new tools here using 'please -newtool <description>'
//
// BEST PRACTICES:
// 1. Always add sensible defaults (e.g., depth=3 for tree, limit=100 for lists)
// 2. Truncate outputs > 50KB with "... (truncated)" message
// 3. Handle all errors gracefully with descriptive error messages
// 4. Use optional parameters with defaults, not required params when possible
// 5. Print dim status messages: fmt.Printf("\033[90m> action: %s\033[0m\n", param)
// 6. For external commands, use exec.Command with proper error handling
// 7. For HTTP requests, set reasonable timeouts (10-30s) and max response sizes (1MB)

var userTools = []Tool{
	// Custom tools will be added here by 'please -newtool'
	// Example:
	// {
	//     Type: "function",
	//     Function: struct {
	//         Name        string                 `json:"name"`
	//         Description string                 `json:"description"`
	//         Parameters  map[string]interface{} `json:"parameters"`
	//     }{
	//         Name:        "example_tool",
	//         Description: "Example tool description",
	//         Parameters: map[string]interface{}{
	//             "type": "object",
	//             "properties": map[string]interface{}{
	//                 "param1": map[string]interface{}{"type": "string", "description": "Parameter description"},
	//             },
	//             "required": []string{},
	//         },
	//     },
	// },
}

// executeUserTool handles execution of custom tools
// Returns (result, handled) where handled=true if this was a user tool
func executeUserTool(name string, argsJSON string) (string, bool) {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %v", err), true
	}

	switch name {
	// Custom tool cases will be added here by 'please -newtool'
	// Example:
	// case "example_tool":
	//     param := args["param1"].(string)
	//     fmt.Printf("\033[90m> example: %s\033[0m\n", param)
	//     return "Result here", true

	default:
		return "", false // Not a user tool
	}
}
