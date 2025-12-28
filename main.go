package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
const defaultModel = "anthropic/claude-3.5-sonnet"
const maxIterations = 20
const sessionTimeout = 5 * time.Minute
const maxModelHistory = 9

// Spinner for loading states
type Spinner struct {
	frames  []string
	current int
	stop    chan bool
	done    chan bool
	stopped bool
}

func NewSpinner() *Spinner {
	return &Spinner{
		frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		stop:   make(chan bool, 1),
		done:   make(chan bool, 1),
	}
}

func (s *Spinner) Start() {
	go func() {
		for {
			select {
			case <-s.stop:
				fmt.Print("\r\033[K") // Clear line
				s.done <- true
				return
			default:
				fmt.Printf("\r\033[90m%s thinking...\033[0m", s.frames[s.current])
				s.current = (s.current + 1) % len(s.frames)
				time.Sleep(80 * time.Millisecond)
			}
		}
	}()
}

func (s *Spinner) Stop() {
	if s.stopped {
		return
	}
	s.stopped = true
	s.stop <- true
	<-s.done
}

var homeDir, _ = os.UserHomeDir()
var configDir = filepath.Join(homeDir, ".please")
var configFile = filepath.Join(configDir, "config.json")
var backupDir = filepath.Join(configDir, "backup")
var sessionFile = filepath.Join(os.TempDir(), "please-session.json")

// Get source directory (where the binary is built from)
var sourceDir = filepath.Join(homeDir, "please")

type Config struct {
	APIKey        string          `json:"api_key"`
	Model         string          `json:"model,omitempty"`
	ModelHistory  []string        `json:"model_history,omitempty"`
	DisabledTools map[string]bool `json:"disabled_tools,omitempty"`
	Endpoint      string          `json:"endpoint,omitempty"` // Custom API endpoint (for Ollama, etc.)
}

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

func saveConfig(config *Config) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configFile, data, 0600)
}

func getAPIKey() string {
	// First try config file
	if config, err := loadConfig(); err == nil && config.APIKey != "" {
		return config.APIKey
	}

	// Fall back to environment variable
	return os.Getenv("OPENROUTER_API_KEY")
}

func getModel() string {
	if config, err := loadConfig(); err == nil && config.Model != "" {
		return config.Model
	}
	return defaultModel
}

func getEndpoint() string {
	if config, err := loadConfig(); err == nil && config.Endpoint != "" {
		return config.Endpoint
	}
	return openRouterURL
}

func isOllama() bool {
	endpoint := getEndpoint()
	return strings.Contains(endpoint, "localhost") || strings.Contains(endpoint, "127.0.0.1")
}

func addModelToHistory(modelSlug string) {
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}

	// Don't add if it's already the current model
	if config.Model == modelSlug {
		return
	}

	// Remove from history if already present
	newHistory := []string{}
	for _, m := range config.ModelHistory {
		if m != modelSlug {
			newHistory = append(newHistory, m)
		}
	}

	// Add current model to history (if set)
	if config.Model != "" && config.Model != modelSlug {
		newHistory = append([]string{config.Model}, newHistory...)
	}

	// Trim to max history
	if len(newHistory) > maxModelHistory {
		newHistory = newHistory[:maxModelHistory]
	}

	config.ModelHistory = newHistory
	config.Model = modelSlug
	saveConfig(config)
}

func runSetup() {
	fmt.Println("please setup")
	fmt.Println("------------")
	fmt.Print("Enter your OpenRouter API key: ")

	var apiKey string
	fmt.Scanln(&apiKey)

	if apiKey == "" {
		fmt.Println("No API key provided. Setup cancelled.")
		os.Exit(1)
	}

	// Preserve existing config fields
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}
	config.APIKey = apiKey

	if err := saveConfig(config); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("API key saved to ~/.please/config.json")
}

type Session struct {
	Messages  []Message `json:"messages"`
	UpdatedAt time.Time `json:"updated_at"`
	Cwd       string    `json:"cwd"`
}

func loadSession() (*Session, error) {
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return nil, err
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}

	// Check if session expired
	if time.Since(session.UpdatedAt) > sessionTimeout {
		os.Remove(sessionFile)
		return nil, fmt.Errorf("session expired")
	}

	return &session, nil
}

func saveSession(messages []Message, cwd string) error {
	session := Session{
		Messages:  messages,
		UpdatedAt: time.Now(),
		Cwd:       cwd,
	}

	data, err := json.Marshal(session)
	if err != nil {
		return err
	}

	return os.WriteFile(sessionFile, data, 0600)
}

func clearSession() {
	os.Remove(sessionFile)
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    int    `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
	} `json:"function"`
}

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools"`
	Stream   bool      `json:"stream,omitempty"`
}

type StreamDelta struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type StreamChoice struct {
	Delta        StreamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type StreamResponse struct {
	Choices []StreamChoice `json:"choices"`
}

type Response struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var tools = []Tool{
	{
		Type: "function",
		Function: struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}{
			Name:        "ask_user",
			Description: "Ask the user a question with up to 3 options. Option 4 is always 'Other' for custom input. Use this when you need clarification or want the user to choose between approaches.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"question": map[string]interface{}{"type": "string", "description": "The question to ask"},
					"option1":  map[string]interface{}{"type": "string", "description": "First option"},
					"option2":  map[string]interface{}{"type": "string", "description": "Second option (optional)"},
					"option3":  map[string]interface{}{"type": "string", "description": "Third option (optional)"},
				},
				"required": []string{"question", "option1"},
			},
		},
	},
	{
		Type: "function",
		Function: struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}{
			Name:        "execute_command",
			Description: "Execute a shell command and return its output",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string", "description": "The shell command to execute"},
				},
				"required": []string{"command"},
			},
		},
	},
	{
		Type: "function",
		Function: struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}{
			Name:        "read_file",
			Description: "Read the contents of a file",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "description": "Path to the file to read"},
				},
				"required": []string{"path"},
			},
		},
	},
	{
		Type: "function",
		Function: struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}{
			Name:        "write_file",
			Description: "Write content to a file",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]interface{}{"type": "string", "description": "Path to the file to write"},
					"content": map[string]interface{}{"type": "string", "description": "Content to write"},
				},
				"required": []string{"path", "content"},
			},
		},
	},
	{
		Type: "function",
		Function: struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}{
			Name:        "list_files",
			Description: "List files in a directory",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "description": "Directory path to list"},
				},
				"required": []string{"path"},
			},
		},
	},
	{
		Type: "function",
		Function: struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}{
			Name:        "grep_files",
			Description: "Search for a pattern in files using ripgrep",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern": map[string]interface{}{"type": "string", "description": "The regex pattern to search for"},
					"path":    map[string]interface{}{"type": "string", "description": "Directory or file to search in"},
				},
				"required": []string{"pattern"},
			},
		},
	},
	{
		Type: "function",
		Function: struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}{
			Name:        "tree",
			Description: "Show directory structure as a tree",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":  map[string]interface{}{"type": "string", "description": "Directory to show tree for"},
					"depth": map[string]interface{}{"type": "integer", "description": "Maximum depth (default: 3)"},
				},
				"required": []string{},
			},
		},
	},
}

// getEnabledTools returns tools that are not disabled in config (core + user tools)
func getEnabledTools() []Tool {
	config, _ := loadConfig()

	// Combine core tools and user tools
	allTools := append([]Tool{}, tools...)
	allTools = append(allTools, userTools...)

	if config == nil || config.DisabledTools == nil {
		return allTools
	}

	enabled := []Tool{}
	for _, t := range allTools {
		if !config.DisabledTools[t.Function.Name] {
			enabled = append(enabled, t)
		}
	}
	return enabled
}

func executeTool(name string, argsJSON string) string {
	// Try user tools first
	if result, handled := executeUserTool(name, argsJSON); handled {
		return result
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %v", err)
	}

	switch name {
	case "ask_user":
		question := args["question"].(string)
		fmt.Printf("\n\033[1;36m%s\033[0m\n\n", question)

		options := []string{}
		if opt, ok := args["option1"].(string); ok && opt != "" {
			options = append(options, opt)
		}
		if opt, ok := args["option2"].(string); ok && opt != "" {
			options = append(options, opt)
		}
		if opt, ok := args["option3"].(string); ok && opt != "" {
			options = append(options, opt)
		}

		// Show options 1-3, option 4 is always "Other"
		for i, opt := range options {
			fmt.Printf("  \033[1;33m%d)\033[0m %s\n", i+1, opt)
		}
		fmt.Printf("  \033[1;33m4)\033[0m Other...\n")
		fmt.Println()
		fmt.Print("\033[90m[1-3: select, 4: other, q/esc/enter: cancel]\033[0m ")

		// Read single character
		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			// Fallback to line-based input
			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)
			if input == "q" || input == "" {
				fmt.Println("\nCancelled.")
				os.Exit(0)
			}
			if len(input) == 1 && input[0] >= '1' && input[0] <= '3' {
				idx := int(input[0] - '1')
				if idx < len(options) {
					return fmt.Sprintf("User selected: %s", options[idx])
				}
			}
			return fmt.Sprintf("User response: %s", input)
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)

		for {
			var buf [1]byte
			os.Stdin.Read(buf[:])
			ch := buf[0]

			// q, space, enter, or esc - quit
			if ch == 'q' || ch == ' ' || ch == '\r' || ch == '\n' || ch == 27 {
				term.Restore(int(os.Stdin.Fd()), oldState)
				fmt.Println("\nCancelled.")
				os.Exit(0)
			}

			// 1, 2, 3 - select option directly
			if ch >= '1' && ch <= '3' {
				idx := int(ch - '1')
				if idx < len(options) {
					term.Restore(int(os.Stdin.Fd()), oldState)
					fmt.Printf("%c\n", ch)
					return fmt.Sprintf("User selected: %s", options[idx])
				}
			}

			// 4 - other, prompt for custom input
			if ch == '4' {
				term.Restore(int(os.Stdin.Fd()), oldState)
				fmt.Println("4")
				fmt.Print("Type your response: ")
				reader := bufio.NewReader(os.Stdin)
				input, err := reader.ReadString('\n')
				if err != nil {
					return "Error reading input"
				}
				input = strings.TrimSpace(input)
				if input == "" {
					fmt.Println("Cancelled.")
					os.Exit(0)
				}
				return fmt.Sprintf("User response: %s", input)
			}

			// Any other key - ignore, keep waiting
		}

	case "execute_command":
		cmdStr := args["command"].(string)
		fmt.Printf("\033[90m> %s\033[0m\n", cmdStr)

		cmd := exec.Command("bash", "-c", cmdStr)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Sprintf("Error creating stdout pipe: %v", err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return fmt.Sprintf("Error creating stderr pipe: %v", err)
		}

		if err := cmd.Start(); err != nil {
			return fmt.Sprintf("Error starting command: %v", err)
		}

		var output strings.Builder
		var wg sync.WaitGroup
		var mu sync.Mutex

		streamPipe := func(pipe io.Reader) {
			defer wg.Done()
			scanner := bufio.NewScanner(pipe)
			// Increase buffer size for long lines
			buf := make([]byte, 0, 64*1024)
			scanner.Buffer(buf, 1024*1024)
			for scanner.Scan() {
				line := scanner.Text()
				fmt.Printf("\033[90m  %s\033[0m\n", line)
				mu.Lock()
				output.WriteString(line + "\n")
				mu.Unlock()
			}
		}

		wg.Add(2)
		go streamPipe(stdout)
		go streamPipe(stderr)

		wg.Wait()
		cmdErr := cmd.Wait()

		if cmdErr != nil {
			return output.String() + "Error: " + cmdErr.Error()
		}
		return output.String()

	case "read_file":
		path := args["path"].(string)
		fmt.Printf("\033[90m> read: %s\033[0m\n", path)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return string(data)

	case "write_file":
		path := args["path"].(string)
		content := args["content"].(string)
		fmt.Printf("\033[90m> write: %s\033[0m\n", path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Sprintf("Error creating directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return "Successfully wrote to " + path

	case "list_files":
		path := args["path"].(string)
		fmt.Printf("\033[90m> ls: %s\033[0m\n", path)
		out, err := exec.Command("ls", "-la", path).CombinedOutput()
		if err != nil {
			return string(out) + "\nError: " + err.Error()
		}
		return string(out)

	case "grep_files":
		pattern := args["pattern"].(string)
		path := "."
		if p, ok := args["path"].(string); ok && p != "" {
			path = p
		}
		fmt.Printf("\033[90m> rg: %s in %s\033[0m\n", pattern, path)
		out, _ := exec.Command("rg", "--color=never", "-n", pattern, path).CombinedOutput()
		return string(out)

	case "tree":
		path := "."
		if p, ok := args["path"].(string); ok && p != "" {
			path = p
		}
		depth := "3"
		if d, ok := args["depth"].(float64); ok {
			depth = fmt.Sprintf("%d", int(d))
		}
		fmt.Printf("\033[90m> tree: %s (depth %s)\033[0m\n", path, depth)
		out, _ := exec.Command("tree", "-L", depth, path).CombinedOutput()
		return string(out)

	default:
		return "Unknown tool: " + name
	}
}

func sendStreamingRequest(messages []Message, apiKey string, spinner *Spinner) (*Message, error) {
	req := Request{
		Model:    getModel(),
		Messages: messages,
		Tools:    getEnabledTools(),
		Stream:   true,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	endpoint := getEndpoint()
	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if !isOllama() {
		// Only set auth headers for remote APIs
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("HTTP-Referer", "https://github.com/Gabriel-Feang/please")
		httpReq.Header.Set("X-Title", "please")
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// Accumulate the response
	var fullContent strings.Builder
	var toolCalls []ToolCall
	toolCallMap := make(map[int]*ToolCall)

	reader := bufio.NewReader(resp.Body)
	firstContent := true

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		line = strings.TrimSpace(line)
		if line == "" || line == "data: [DONE]" {
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		var streamResp StreamResponse
		if err := json.Unmarshal([]byte(data), &streamResp); err != nil {
			continue
		}

		if len(streamResp.Choices) == 0 {
			continue
		}

		delta := streamResp.Choices[0].Delta

		// Stream content to stdout
		if delta.Content != "" {
			if firstContent {
				if spinner != nil {
					spinner.Stop()
				}
				firstContent = false
			}
			fmt.Print(delta.Content)
			fullContent.WriteString(delta.Content)
		}

		// Accumulate tool calls
		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			if existing, ok := toolCallMap[idx]; ok {
				existing.Function.Arguments += tc.Function.Arguments
			} else {
				newTC := ToolCall{
					ID:    tc.ID,
					Type:  tc.Type,
					Index: idx,
				}
				newTC.Function.Name = tc.Function.Name
				newTC.Function.Arguments = tc.Function.Arguments
				toolCallMap[idx] = &newTC
			}
		}
	}

	// Convert tool call map to slice
	for _, tc := range toolCallMap {
		toolCalls = append(toolCalls, *tc)
	}

	// Add newline after streamed content
	if fullContent.Len() > 0 {
		fmt.Println()
	}

	return &Message{
		Role:      "assistant",
		Content:   fullContent.String(),
		ToolCalls: toolCalls,
	}, nil
}

func runAgent(messages []Message, apiKey string, cwd string) []Message {
	for i := 0; i < maxIterations; i++ {
		// Show spinner while waiting for first token
		spinner := NewSpinner()
		spinner.Start()

		msg, err := sendStreamingRequest(messages, apiKey, spinner)

		// Stop spinner if not already stopped (e.g., tool-only response)
		spinner.Stop()

		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

		// Check for tool calls
		if len(msg.ToolCalls) == 0 {
			messages = append(messages, *msg)
			break
		}

		// Add assistant message
		messages = append(messages, *msg)

		// Execute tools
		for _, tc := range msg.ToolCalls {
			result := executeTool(tc.Function.Name, tc.Function.Arguments)

			// Truncate if too long
			if len(result) > 50000 {
				result = result[:50000] + "... (truncated)"
			}

			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}
	}

	return messages
}

func runHelp() {
	fmt.Println(`please - A fast, agentic terminal assistant powered by AI

USAGE:
  please <prompt>       Start a new conversation
  hmm <follow-up>       Continue the last conversation
  pls / plz             Aliases for 'please'

FLAGS:
  -setup                Configure your OpenRouter API key
  -help                 Show this help message
  -tools                Toggle tools on/off interactively
  -model [slug]         Switch model (interactive if no slug provided)
  -ollama               Use local Ollama (no API key needed)
  -openrouter           Use OpenRouter API (default)
  -endpoint [url]       Set custom API endpoint
  -newtool <prompt>     Create a new custom tool via AI
  -rollback             Restore from last backup (before -newtool)
  -reset                Factory reset (wipes all config and custom tools)

OLLAMA:
  please -ollama
  please -model llama3.1

TOOLS:`)
	for _, t := range tools {
		fmt.Printf("  %-18s %s\n", t.Function.Name, t.Function.Description)
	}
	if len(userTools) > 0 {
		fmt.Println("\nCUSTOM TOOLS:")
		for _, t := range userTools {
			fmt.Printf("  %-18s %s\n", t.Function.Name, t.Function.Description)
		}
	}
	fmt.Println(`
EXAMPLES:
  please list all large files in this directory
  please what processes are using port 3000
  hmm now kill those processes

CONFIG:
  ~/.please/config.json   API key, model, disabled tools

Get your API key at: https://openrouter.ai/keys`)
}

func runTools() {
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}
	if config.DisabledTools == nil {
		config.DisabledTools = make(map[string]bool)
	}

	// Combine all tools for display
	allTools := append([]Tool{}, tools...)
	allTools = append(allTools, userTools...)

	fmt.Println("Toggle tools on/off (press number to toggle, q/esc/enter to save and exit)")
	fmt.Println()

	printTools := func() {
		for i, t := range allTools {
			status := "\033[32m✓\033[0m"
			if config.DisabledTools[t.Function.Name] {
				status = "\033[90m✗\033[0m"
			}
			// Mark custom tools
			marker := ""
			if i >= len(tools) {
				marker = " \033[36m(custom)\033[0m"
			}
			fmt.Printf("  %d) [%s] %-18s %s%s\n", i+1, status, t.Function.Name, t.Function.Description, marker)
		}
	}

	printTools()
	fmt.Println()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println("Error: terminal not supported")
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	for {
		var buf [1]byte
		os.Stdin.Read(buf[:])
		ch := buf[0]

		// Exit keys
		if ch == 'q' || ch == 27 || ch == '\r' || ch == '\n' {
			term.Restore(int(os.Stdin.Fd()), oldState)
			saveConfig(config)
			fmt.Println("\nSaved!")
			return
		}

		// Toggle tool by number (1-9 and a-z for more)
		idx := -1
		if ch >= '1' && ch <= '9' {
			idx = int(ch - '1')
		} else if ch >= 'a' && ch <= 'z' {
			idx = int(ch-'a') + 9
		}

		if idx >= 0 && idx < len(allTools) {
			name := allTools[idx].Function.Name
			config.DisabledTools[name] = !config.DisabledTools[name]
			// Redraw
			fmt.Print("\033[" + fmt.Sprintf("%d", len(allTools)+1) + "A") // Move up
			fmt.Print("\033[J") // Clear below
			printTools()
			fmt.Println()
		}
	}
}

func runEndpoint(url string) {
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}

	config.Endpoint = url
	if err := saveConfig(config); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}

	if isOllama() {
		fmt.Printf("Endpoint set to: %s (Ollama mode - no API key needed)\n", url)
		fmt.Println("\nMake sure Ollama is running with a model that supports tools:")
		fmt.Println("  ollama run llama3.1")
		fmt.Println("  ollama run qwen2.5")
		fmt.Println("  ollama run mistral")
	} else {
		fmt.Printf("Endpoint set to: %s\n", url)
	}
}

func setupOllama() {
	fmt.Println("Setting up Ollama...")

	// Check if ollama is installed
	if _, err := exec.LookPath("ollama"); err != nil {
		fmt.Println("Ollama not found. Installing via Homebrew...")
		cmd := exec.Command("brew", "install", "ollama")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Printf("Error installing ollama: %v\n", err)
			fmt.Println("\nPlease install manually: brew install ollama")
			os.Exit(1)
		}
		fmt.Println("Ollama installed successfully!")
	} else {
		fmt.Println("✓ Ollama is installed")
	}

	// Start ollama service if not running
	fmt.Println("Starting Ollama service...")
	startCmd := exec.Command("brew", "services", "start", "ollama")
	startCmd.Run() // Ignore error - might already be running

	// Wait a moment for service to start
	time.Sleep(2 * time.Second)

	// Check if llama3.1 model is available
	listCmd := exec.Command("ollama", "list")
	output, err := listCmd.Output()
	if err != nil || !strings.Contains(string(output), "llama3.1") {
		fmt.Println("Pulling llama3.1 model (this may take a few minutes)...")
		pullCmd := exec.Command("ollama", "pull", "llama3.1")
		pullCmd.Stdout = os.Stdout
		pullCmd.Stderr = os.Stderr
		if err := pullCmd.Run(); err != nil {
			fmt.Printf("Error pulling model: %v\n", err)
			fmt.Println("\nPlease pull manually: ollama pull llama3.1")
			os.Exit(1)
		}
		fmt.Println("Model pulled successfully!")
	} else {
		fmt.Println("✓ llama3.1 model is available")
	}

	// Set config for Ollama
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}
	config.Endpoint = "http://localhost:11434/v1/chat/completions"
	config.Model = "llama3.1"

	if err := saveConfig(config); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}
	addModelToHistory("llama3.1")

	fmt.Println("\n✓ Ollama setup complete!")
	fmt.Println("  Endpoint: http://localhost:11434/v1/chat/completions")
	fmt.Println("  Model: llama3.1")
	fmt.Println("\nYou can now use: please <your prompt>")
}

func runModel(slug string) {
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}

	if slug != "" {
		// Direct model switch
		addModelToHistory(slug)
		fmt.Printf("Switched to model: %s\n", slug)
		return
	}

	// Interactive mode
	fmt.Println("Select a model (press number, 0 to keep current, q/esc to cancel)")
	fmt.Println()

	current := config.Model
	if current == "" {
		current = defaultModel
	}
	fmt.Printf("  \033[1;32m0)\033[0m %s (current)\n", current)

	for i, m := range config.ModelHistory {
		if i >= 9 {
			break
		}
		fmt.Printf("  \033[1;33m%d)\033[0m %s\n", i+1, m)
	}
	fmt.Println()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println("Error: terminal not supported")
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	for {
		var buf [1]byte
		os.Stdin.Read(buf[:])
		ch := buf[0]

		if ch == 'q' || ch == 27 {
			term.Restore(int(os.Stdin.Fd()), oldState)
			fmt.Println("\nCancelled.")
			return
		}

		if ch == '0' || ch == '\r' || ch == '\n' {
			term.Restore(int(os.Stdin.Fd()), oldState)
			fmt.Printf("\nKeeping: %s\n", current)
			return
		}

		if ch >= '1' && ch <= '9' {
			idx := int(ch - '1')
			if idx < len(config.ModelHistory) {
				term.Restore(int(os.Stdin.Fd()), oldState)
				selected := config.ModelHistory[idx]
				addModelToHistory(selected)
				fmt.Printf("\nSwitched to: %s\n", selected)
				return
			}
		}
	}
}

func runRollback() {
	// Check if backup exists
	mainBackup := filepath.Join(backupDir, "main.go.bak")
	toolsBackup := filepath.Join(backupDir, "tools.go.bak")
	timestampFile := filepath.Join(backupDir, "timestamp.txt")

	if _, err := os.Stat(mainBackup); os.IsNotExist(err) {
		fmt.Println("No backup found. Nothing to rollback.")
		return
	}

	// Show backup info
	if data, err := os.ReadFile(timestampFile); err == nil {
		fmt.Printf("Backup from: %s\n", strings.TrimSpace(string(data)))
	}

	fmt.Println("Restoring from backup...")

	// Restore main.go
	if data, err := os.ReadFile(mainBackup); err == nil {
		if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), data, 0644); err != nil {
			fmt.Printf("Error restoring main.go: %v\n", err)
			return
		}
		fmt.Println("  Restored: main.go")
	}

	// Restore tools.go if it exists
	if data, err := os.ReadFile(toolsBackup); err == nil {
		if err := os.WriteFile(filepath.Join(sourceDir, "tools.go"), data, 0644); err != nil {
			fmt.Printf("Error restoring tools.go: %v\n", err)
			return
		}
		fmt.Println("  Restored: tools.go")
	}

	// Rebuild
	fmt.Println("Rebuilding...")
	cmd := exec.Command("go", "build", "-o", "please", ".")
	cmd.Dir = sourceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("Build failed: %s\n", string(out))
		return
	}

	fmt.Println("Rollback complete!")
}

func runReset() {
	fmt.Println("\033[1;31m⚠ WARNING: This will completely wipe Please!\033[0m")
	fmt.Println()
	fmt.Println("This will delete:")
	fmt.Println("  - All custom tools")
	fmt.Println("  - Your configuration (API key, model settings)")
	fmt.Println("  - All backups")
	fmt.Println()
	fmt.Println("Press \033[1m1\033[0m to confirm, any other key to cancel...")

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println("Error: terminal not supported")
		return
	}

	var buf [1]byte
	os.Stdin.Read(buf[:])
	term.Restore(int(os.Stdin.Fd()), oldState)

	if buf[0] != '1' {
		fmt.Println("\nCancelled.")
		return
	}

	fmt.Println("1")
	fmt.Print("\nType 'reset' and press Enter to confirm: ")

	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input != "reset" {
		fmt.Println("Cancelled.")
		return
	}

	fmt.Println()
	fmt.Println("Resetting...")

	// Remove config directory
	if err := os.RemoveAll(configDir); err != nil {
		fmt.Printf("Error removing config: %v\n", err)
	} else {
		fmt.Println("  Removed: ~/.please/")
	}

	// Remove custom tools.go if exists
	toolsFile := filepath.Join(sourceDir, "tools.go")
	if _, err := os.Stat(toolsFile); err == nil {
		if err := os.Remove(toolsFile); err != nil {
			fmt.Printf("Error removing tools.go: %v\n", err)
		} else {
			fmt.Println("  Removed: tools.go")
		}
	}

	// Rebuild
	fmt.Println("Rebuilding...")
	cmd := exec.Command("go", "build", "-o", "please", ".")
	cmd.Dir = sourceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("Build failed: %s\n", string(out))
		return
	}

	fmt.Println()
	fmt.Println("Reset complete! Run 'please -setup' to configure.")
}

func createBackup() error {
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return err
	}

	// Backup main.go
	mainSrc := filepath.Join(sourceDir, "main.go")
	if data, err := os.ReadFile(mainSrc); err == nil {
		if err := os.WriteFile(filepath.Join(backupDir, "main.go.bak"), data, 0644); err != nil {
			return err
		}
	}

	// Backup tools.go if exists
	toolsSrc := filepath.Join(sourceDir, "tools.go")
	if data, err := os.ReadFile(toolsSrc); err == nil {
		if err := os.WriteFile(filepath.Join(backupDir, "tools.go.bak"), data, 0644); err != nil {
			return err
		}
	}

	// Save timestamp
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	os.WriteFile(filepath.Join(backupDir, "timestamp.txt"), []byte(timestamp), 0644)

	return nil
}

const newtoolSystemPrompt = `You are adding a new tool to the Please CLI. You will modify tools.go to add the requested tool.

CURRENT tools.go STRUCTURE:
The file exports two things:
1. userTools []Tool - slice of custom tool definitions
2. executeUserTool(name, argsJSON string) string - switch statement for tool execution

BEST PRACTICES:
1. Always add sensible defaults (e.g., depth=3 for tree, limit=100 for lists)
2. Truncate outputs > 50KB with "... (truncated)" message
3. Handle all errors gracefully with descriptive error messages
4. Use optional parameters with defaults, not required params when possible
5. Print dim status messages showing what's running: fmt.Printf("\033[90m> action: %s\033[0m\n", param)
6. For external commands, use exec.Command with proper error handling
7. For HTTP requests, set reasonable timeouts (10-30s) and max response sizes (1MB)

TOOL DEFINITION FORMAT:
{
    Type: "function",
    Function: struct {
        Name        string
        Description string
        Parameters  map[string]interface{}
    }{
        Name:        "tool_name",
        Description: "What this tool does",
        Parameters: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "param1": map[string]interface{}{"type": "string", "description": "..."},
            },
            "required": []string{"param1"},
        },
    },
}

EXECUTION FORMAT:
case "tool_name":
    param := args["param1"].(string)
    fmt.Printf("\033[90m> doing: %s\033[0m\n", param)
    // Implementation here
    return result

IMPORTANT:
- Read the current tools.go content to understand existing patterns
- Add your new tool to the userTools slice
- Add the execution case to executeUserTool switch
- Use read_file and write_file tools to modify tools.go
- After modifying, the system will automatically rebuild`

func runNewtool(prompt string) {
	apiKey := getAPIKey()
	if apiKey == "" {
		fmt.Println("Error: No API key configured")
		fmt.Println("Run 'please -setup' to configure your OpenRouter API key")
		os.Exit(1)
	}

	// Create backup first
	fmt.Println("Creating backup...")
	if err := createBackup(); err != nil {
		fmt.Printf("Error creating backup: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  Backup saved to ~/.please/backup/")

	// Ensure tools.go exists
	toolsFile := filepath.Join(sourceDir, "tools.go")
	if _, err := os.Stat(toolsFile); os.IsNotExist(err) {
		// Create initial tools.go from template
		template := `package main

// User-defined custom tools
// Add new tools here using 'please -newtool <description>'

var userTools = []Tool{
	// Custom tools will be added here
}

func executeUserTool(name string, argsJSON string) (string, bool) {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %v", err), true
	}

	switch name {
	// Custom tool cases will be added here
	default:
		return "", false // Not a user tool
	}
}
`
		if err := os.WriteFile(toolsFile, []byte(template), 0644); err != nil {
			fmt.Printf("Error creating tools.go: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("  Created tools.go template")
	}

	// Read current tools.go
	toolsContent, err := os.ReadFile(toolsFile)
	if err != nil {
		fmt.Printf("Error reading tools.go: %v\n", err)
		os.Exit(1)
	}

	cwd, _ := os.Getwd()

	// Prepare messages
	systemPrompt := fmt.Sprintf(`%s

Current tools.go content:
%s

Working directory: %s
Source directory: %s`, newtoolSystemPrompt, string(toolsContent), cwd, sourceDir)

	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: fmt.Sprintf("Create a new tool: %s", prompt)},
	}

	fmt.Println()
	fmt.Println("Creating tool...")

	// Run the agent
	messages = runAgent(messages, apiKey, cwd)

	// Rebuild
	fmt.Println()
	fmt.Println("Rebuilding...")
	cmd := exec.Command("go", "build", "-o", "please", ".")
	cmd.Dir = sourceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("Build failed: %s\n", string(out))
		fmt.Println("Use 'please -rollback' to restore the previous version")
		os.Exit(1)
	}

	// Update symlinks
	for _, alias := range []string{"hmm", "pls", "plz"} {
		os.Remove(filepath.Join(sourceDir, alias))
		os.Symlink("please", filepath.Join(sourceDir, alias))
	}

	fmt.Println("Done! New tool added successfully.")
	fmt.Println("Use 'please -rollback' if something went wrong.")
}

func main() {
	// Determine mode based on binary name
	binName := filepath.Base(os.Args[0])
	isHmm := binName == "hmm"

	// Handle flags (only for 'please', not 'hmm')
	if !isHmm && len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-setup":
			runSetup()
			return
		case "-help", "--help", "-h":
			runHelp()
			return
		case "-tools":
			runTools()
			return
		case "-model":
			slug := ""
			if len(os.Args) >= 3 {
				slug = os.Args[2]
			}
			runModel(slug)
			return
		case "-rollback":
			runRollback()
			return
		case "-reset":
			runReset()
			return
		case "-newtool":
			if len(os.Args) < 3 {
				fmt.Println("Usage: please -newtool <description of the tool you want>")
				os.Exit(1)
			}
			runNewtool(strings.Join(os.Args[2:], " "))
			return
		case "-endpoint":
			if len(os.Args) < 3 {
				// Show current endpoint
				fmt.Printf("Current endpoint: %s\n", getEndpoint())
				fmt.Println("\nUsage: please -endpoint <url>")
				fmt.Println("\nOr use shortcuts:")
				fmt.Println("  please -ollama      # Use local Ollama")
				fmt.Println("  please -openrouter  # Use OpenRouter (default)")
				return
			}
			runEndpoint(os.Args[2])
			return
		case "-ollama":
			setupOllama()
			return
		case "-openrouter":
			runEndpoint(openRouterURL)
			return
		}
	}

	apiKey := getAPIKey()
	if apiKey == "" && !isOllama() {
		fmt.Println("Error: No API key configured")
		fmt.Println("Run 'please -setup' to configure your OpenRouter API key")
		fmt.Println("Or use Ollama: please -endpoint http://localhost:11434/v1/chat/completions")
		os.Exit(1)
	}

	cwd, _ := os.Getwd()

	var messages []Message

	if isHmm {
		// Continue mode - load existing session
		session, err := loadSession()
		if err != nil {
			fmt.Println("No active session. Run 'please' first to start a conversation.")
			os.Exit(1)
		}

		var prompt string
		if len(os.Args) < 2 || os.Args[1] == "-" {
			// Interactive prompt
			fmt.Print("hmm> ")
			reader := bufio.NewReader(os.Stdin)
			line, err := reader.ReadString('\n')
			if err != nil {
				fmt.Println("Error reading input")
				os.Exit(1)
			}
			prompt = strings.TrimSpace(line)
			if prompt == "" {
				fmt.Println("Usage: hmm <follow-up message>")
				os.Exit(1)
			}
		} else {
			prompt = strings.Join(os.Args[1:], " ")
		}

		messages = session.Messages
		messages = append(messages, Message{Role: "user", Content: prompt})

	} else {
		// New session mode
		var prompt string

		if len(os.Args) < 2 {
			// No args - show interactive prompt
			fmt.Print("please> ")
			reader := bufio.NewReader(os.Stdin)
			line, err := reader.ReadString('\n')
			if err != nil {
				fmt.Println("Error reading input")
				os.Exit(1)
			}
			prompt = strings.TrimSpace(line)
			if prompt == "" {
				fmt.Println("Usage: please <prompt>")
				fmt.Println("       please -help   (show all options)")
				os.Exit(1)
			}
		} else if os.Args[1] == "-" {
			// Read from stdin (for special characters)
			fmt.Print("please> ")
			reader := bufio.NewReader(os.Stdin)
			line, err := reader.ReadString('\n')
			if err != nil {
				fmt.Println("Error reading input")
				os.Exit(1)
			}
			prompt = strings.TrimSpace(line)
		} else {
			prompt = strings.Join(os.Args[1:], " ")
		}

		// Clear any existing session
		clearSession()

		systemPrompt := fmt.Sprintf(`You are a helpful terminal assistant with full shell access.

Tools:
- ask_user: MUST use this to ask questions. Never write questions as plain text.
- execute_command: Run any shell command
- read_file / write_file / list_files / grep_files / tree

Rules:
- NEVER write questions or options as plain text. ALWAYS use ask_user tool instead.
- Be proactive - execute commands, don't just talk about them
- To open files: 'open <path>' (macOS)
- Use 'rg' and 'fd', never grep/glob/find
- Be concise - minimal output

Please CLI Commands (tell users about these when relevant):
- 'please -help'      Show all commands and available tools
- 'please -tools'     Toggle tools on/off interactively
- 'please -model'     Switch AI model (saves last 9 used)
- 'please -newtool'   Create custom tools via AI (e.g., 'please -newtool a tool to fetch weather')
- 'please -rollback'  Restore from backup if -newtool breaks something
- 'please -reset'     Factory reset (wipes config and custom tools)
- 'please -setup'     Configure API key

If user asks how to create a tool, add a tool, or extend functionality: tell them to use 'please -newtool <description>'.
If user asks about available commands or help: tell them to run 'please -help'.

Current directory: %s`, cwd)

		messages = []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		}
	}

	// Run the agent
	messages = runAgent(messages, apiKey, cwd)

	// Save session for potential follow-up
	if err := saveSession(messages, cwd); err != nil {
		// Non-fatal, just can't continue with hmm
		fmt.Fprintf(os.Stderr, "Warning: couldn't save session: %v\n", err)
	}
}
