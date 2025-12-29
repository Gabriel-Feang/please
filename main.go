package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// Image extensions we support
var imageExtensions = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp"}

// Patterns for finding image paths in text
var (
	// Quoted paths: "path" or 'path'
	quotedPathPattern = regexp.MustCompile(`["']([^"']+\.(?:png|jpg|jpeg|gif|webp|bmp))["']`)
	// Absolute paths with possible escaped spaces: /path/to/file.png or /path/to/file\ name.png
	absolutePathPattern = regexp.MustCompile(`(/(?:[^\s\\]|\\ )+\.(?:png|jpg|jpeg|gif|webp|bmp))`)
	// Home paths: ~/path/to/file.png
	homePathPattern = regexp.MustCompile(`(~(?:[^\s\\]|\\ )+\.(?:png|jpg|jpeg|gif|webp|bmp))`)
)

// isImagePath checks if a path points to an image file
func isImagePath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, imgExt := range imageExtensions {
		if ext == imgExt {
			return true
		}
	}
	return false
}

// expandPath expands ~ and cleans the path
func expandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[1:])
	}
	// Handle escaped spaces (backslash before space)
	path = strings.ReplaceAll(path, "\\ ", " ")
	return filepath.Clean(path)
}

// extractImagesFromPrompt finds image paths in the prompt and returns the cleaned text + image paths
func extractImagesFromPrompt(prompt string) (string, []string) {
	var images []string
	var pathsToRemove []string

	// Try quoted paths first (most reliable)
	for _, match := range quotedPathPattern.FindAllStringSubmatch(prompt, -1) {
		if len(match) > 1 {
			path := match[1]
			expanded := expandPath(path)
			if _, err := os.Stat(expanded); err == nil {
				images = append(images, expanded)
				pathsToRemove = append(pathsToRemove, match[0]) // Include quotes
			}
		}
	}

	// Try absolute paths with escaped spaces
	for _, match := range absolutePathPattern.FindAllStringSubmatch(prompt, -1) {
		if len(match) > 1 {
			path := match[1]
			expanded := expandPath(path)
			// Skip if already found via quoted path
			alreadyFound := false
			for _, img := range images {
				if img == expanded {
					alreadyFound = true
					break
				}
			}
			if !alreadyFound {
				if _, err := os.Stat(expanded); err == nil {
					images = append(images, expanded)
					pathsToRemove = append(pathsToRemove, match[1])
				}
			}
		}
	}

	// Try home paths
	for _, match := range homePathPattern.FindAllStringSubmatch(prompt, -1) {
		if len(match) > 1 {
			path := match[1]
			expanded := expandPath(path)
			alreadyFound := false
			for _, img := range images {
				if img == expanded {
					alreadyFound = true
					break
				}
			}
			if !alreadyFound {
				if _, err := os.Stat(expanded); err == nil {
					images = append(images, expanded)
					pathsToRemove = append(pathsToRemove, match[1])
				}
			}
		}
	}

	// Remove found paths from prompt
	cleanedPrompt := prompt
	for _, p := range pathsToRemove {
		cleanedPrompt = strings.Replace(cleanedPrompt, p, "", 1)
	}

	return strings.TrimSpace(cleanedPrompt), images
}

// encodeImageToBase64 reads an image file and returns a data URI
func encodeImageToBase64(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	// Determine MIME type from extension
	ext := strings.ToLower(filepath.Ext(path))
	var mimeType string
	switch ext {
	case ".png":
		mimeType = "image/png"
	case ".jpg", ".jpeg":
		mimeType = "image/jpeg"
	case ".gif":
		mimeType = "image/gif"
	case ".webp":
		mimeType = "image/webp"
	case ".bmp":
		mimeType = "image/bmp"
	default:
		mimeType = "image/png" // Default fallback
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("data:%s;base64,%s", mimeType, encoded), nil
}

// createMultimodalMessage creates a message with text and images
func createMultimodalMessage(text string, imagePaths []string) Message {
	if len(imagePaths) == 0 {
		return Message{Role: "user", Content: text}
	}

	parts := []ContentPart{}

	// Add text part first (if not empty)
	if text != "" {
		parts = append(parts, ContentPart{Type: "text", Text: text})
	}

	// Add image parts
	for _, imgPath := range imagePaths {
		dataURI, err := encodeImageToBase64(imgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: couldn't read image %s: %v\n", imgPath, err)
			continue
		}
		parts = append(parts, ContentPart{
			Type:     "image_url",
			ImageURL: &ImageURL{URL: dataURI},
		})
	}

	// If we only have text (images failed to load), return simple message
	if len(parts) == 1 && parts[0].Type == "text" {
		return Message{Role: "user", Content: text}
	}

	return Message{Role: "user", ContentParts: parts}
}

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
	fmt.Println(bold("please setup"))
	fmt.Println(muted("─────────────"))
	fmt.Print("Enter your OpenRouter API key: ")

	var apiKey string
	fmt.Scanln(&apiKey)

	if apiKey == "" {
		fmt.Println(warn("No API key provided. Setup cancelled."))
		os.Exit(1)
	}

	// Preserve existing config fields
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}
	config.APIKey = apiKey

	if err := saveConfig(config); err != nil {
		fmt.Println(errorf("Error saving config: " + err.Error()))
		os.Exit(1)
	}

	fmt.Println(success("API key saved to ~/.please/config.json"))
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

// ContentPart represents a part of multimodal content (text or image)
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type Message struct {
	Role         string        `json:"role"`
	Content      string        `json:"-"` // Used internally, marshaled via custom method
	ContentParts []ContentPart `json:"-"` // For multimodal messages
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	Name         string        `json:"name,omitempty"`
}

// MarshalJSON custom marshals Message to handle both string and array content
func (m Message) MarshalJSON() ([]byte, error) {
	type Alias Message
	aux := struct {
		Content interface{} `json:"content,omitempty"`
		Alias
	}{
		Alias: Alias(m),
	}

	if len(m.ContentParts) > 0 {
		aux.Content = m.ContentParts
	} else if m.Content != "" {
		aux.Content = m.Content
	}

	return json.Marshal(aux)
}

// UnmarshalJSON custom unmarshals Message to handle both string and array content
func (m *Message) UnmarshalJSON(data []byte) error {
	type Alias Message
	aux := struct {
		Content json.RawMessage `json:"content"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if len(aux.Content) > 0 {
		// Try to unmarshal as string first
		var str string
		if err := json.Unmarshal(aux.Content, &str); err == nil {
			m.Content = str
			return nil
		}

		// Try as array of content parts
		var parts []ContentPart
		if err := json.Unmarshal(aux.Content, &parts); err == nil {
			m.ContentParts = parts
			// Extract text content for convenience
			for _, p := range parts {
				if p.Type == "text" {
					m.Content += p.Text
				}
			}
		}
	}

	return nil
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
		options = append(options, "Other...")

		selected := RunSelector(question, options)

		if selected == -1 {
			fmt.Println(muted("Cancelled."))
			os.Exit(0)
		}

		// "Other" option selected
		if selected == len(options)-1 {
			fmt.Print(muted("Your response: "))
			reader := bufio.NewReader(os.Stdin)
			input, err := reader.ReadString('\n')
			if err != nil {
				return "Error reading input"
			}
			input = strings.TrimSpace(input)
			if input == "" {
				fmt.Println(muted("Cancelled."))
				os.Exit(0)
			}
			return fmt.Sprintf("User response: %s", input)
		}

		return fmt.Sprintf("User selected: %s", options[selected])

	case "execute_command":
		cmdStr := args["command"].(string)
		fmt.Println(toolPrefixStyle.Render("> " + cmdStr))

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
				fmt.Println(toolOutputStyle.Render("  " + line))
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
		fmt.Println(toolPrefixStyle.Render("> read: " + path))
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return string(data)

	case "write_file":
		path := args["path"].(string)
		content := args["content"].(string)
		fmt.Println(toolPrefixStyle.Render("> write: " + path))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Sprintf("Error creating directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return "Successfully wrote to " + path

	case "list_files":
		path := args["path"].(string)
		fmt.Println(toolPrefixStyle.Render("> ls: " + path))
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
		fmt.Println(toolPrefixStyle.Render(fmt.Sprintf("> rg: %s in %s", pattern, path)))
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
		fmt.Println(toolPrefixStyle.Render(fmt.Sprintf("> tree: %s (depth %s)", path, depth)))
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
	title := boldStyle.Render("please") + muted(" - A fast, agentic terminal assistant powered by AI")
	fmt.Println(title)
	fmt.Println()

	fmt.Println(boldStyle.Render("USAGE"))
	fmt.Printf("  %s       Start a new conversation\n", primaryStyle.Render("please <prompt>"))
	fmt.Printf("  %s       Continue the last conversation\n", primaryStyle.Render("hmm <follow-up>"))
	fmt.Printf("  %s             Aliases for 'please'\n", muted("pls / plz"))
	fmt.Println()

	fmt.Println(boldStyle.Render("FLAGS"))
	fmt.Printf("  %s                %s\n", code("-setup"), "Configure your OpenRouter API key")
	fmt.Printf("  %s                 %s\n", code("-help"), "Show this help message")
	fmt.Printf("  %s                %s\n", code("-tools"), "Toggle tools on/off interactively")
	fmt.Printf("  %s         %s\n", code("-model [slug]"), "Switch model (interactive if no slug)")
	fmt.Printf("  %s               %s\n", code("-ollama"), "Use local Ollama (no API key needed)")
	fmt.Printf("  %s           %s\n", code("-openrouter"), "Use OpenRouter API (default)")
	fmt.Printf("  %s       %s\n", code("-endpoint [url]"), "Set custom API endpoint")
	fmt.Printf("  %s     %s\n", code("-newtool <prompt>"), "Create a new custom tool via AI")
	fmt.Printf("  %s             %s\n", code("-rollback"), "Restore from last backup")
	fmt.Printf("  %s                %s\n", code("-reset"), "Factory reset (wipes all config)")
	fmt.Println()

	fmt.Println(boldStyle.Render("TOOLS"))
	for _, t := range tools {
		fmt.Printf("  %s  %s\n", primaryStyle.Render(fmt.Sprintf("%-16s", t.Function.Name)), muted(t.Function.Description))
	}
	if len(userTools) > 0 {
		fmt.Println()
		fmt.Println(boldStyle.Render("CUSTOM TOOLS"))
		for _, t := range userTools {
			fmt.Printf("  %s  %s\n", primaryStyle.Render(fmt.Sprintf("%-16s", t.Function.Name)), muted(t.Function.Description))
		}
	}
	fmt.Println()

	fmt.Println(boldStyle.Render("EXAMPLES"))
	fmt.Printf("  %s\n", code("please list all large files in this directory"))
	fmt.Printf("  %s\n", code("please what processes are using port 3000"))
	fmt.Printf("  %s\n", code("hmm now kill those processes"))
	fmt.Println()

	fmt.Println(muted("Config: ~/.please/config.json"))
	fmt.Println(muted("API key: https://openrouter.ai/keys"))
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

	// Build toggle items
	items := make([]toggleItem, len(allTools))
	for i, t := range allTools {
		desc := t.Function.Description
		if i >= len(tools) {
			desc += " (custom)"
		}
		items[i] = toggleItem{
			name:    t.Function.Name,
			desc:    desc,
			enabled: !config.DisabledTools[t.Function.Name],
		}
	}

	// Run interactive toggle
	result := RunToggleList("Toggle Tools", items)

	// Update config with results
	for i, item := range result {
		name := allTools[i].Function.Name
		if item.enabled {
			delete(config.DisabledTools, name)
		} else {
			config.DisabledTools[name] = true
		}
	}

	saveConfig(config)
	fmt.Println(success("Saved!"))
}

func runEndpoint(url string) {
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}

	config.Endpoint = url
	if err := saveConfig(config); err != nil {
		fmt.Println(errorf("Error saving config: " + err.Error()))
		os.Exit(1)
	}

	if isOllama() {
		fmt.Println(success("Endpoint set to: " + url))
		fmt.Println(muted("Ollama mode - no API key needed"))
		fmt.Println()
		fmt.Println(muted("Make sure Ollama is running with a model that supports tools:"))
		fmt.Println(muted("  ollama run llama3.1"))
		fmt.Println(muted("  ollama run qwen2.5"))
		fmt.Println(muted("  ollama run mistral"))
	} else {
		fmt.Println(success("Endpoint set to: " + url))
	}
}

func setupOllama() {
	fmt.Println(bold("Setting up Ollama..."))

	// Check if ollama is installed
	if _, err := exec.LookPath("ollama"); err != nil {
		fmt.Println(muted("Ollama not found. Installing via Homebrew..."))
		cmd := exec.Command("brew", "install", "ollama")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Println(errorf("Error installing ollama: " + err.Error()))
			fmt.Println(muted("Please install manually: brew install ollama"))
			os.Exit(1)
		}
		fmt.Println(success("Ollama installed!"))
	} else {
		fmt.Println(success("✓ Ollama is installed"))
	}

	// Start ollama service if not running
	fmt.Println(muted("Starting Ollama service..."))
	startCmd := exec.Command("brew", "services", "start", "ollama")
	startCmd.Run() // Ignore error - might already be running

	// Wait a moment for service to start
	time.Sleep(2 * time.Second)

	// Check if llama3.1 model is available
	listCmd := exec.Command("ollama", "list")
	output, err := listCmd.Output()
	if err != nil || !strings.Contains(string(output), "llama3.1") {
		fmt.Println(muted("Pulling llama3.1 model (this may take a few minutes)..."))
		pullCmd := exec.Command("ollama", "pull", "llama3.1")
		pullCmd.Stdout = os.Stdout
		pullCmd.Stderr = os.Stderr
		if err := pullCmd.Run(); err != nil {
			fmt.Println(errorf("Error pulling model: " + err.Error()))
			fmt.Println(muted("Please pull manually: ollama pull llama3.1"))
			os.Exit(1)
		}
		fmt.Println(success("Model pulled!"))
	} else {
		fmt.Println(success("✓ llama3.1 model is available"))
	}

	// Set config for Ollama
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}
	config.Endpoint = "http://localhost:11434/v1/chat/completions"
	config.Model = "llama3.1"

	if err := saveConfig(config); err != nil {
		fmt.Println(errorf("Error saving config: " + err.Error()))
		os.Exit(1)
	}
	addModelToHistory("llama3.1")

	fmt.Println()
	fmt.Println(success("✓ Ollama setup complete!"))
	fmt.Println(muted("  Endpoint: http://localhost:11434/v1/chat/completions"))
	fmt.Println(muted("  Model: llama3.1"))
	fmt.Println()
	fmt.Println("You can now use: " + code("please <your prompt>"))
}

func runModel(slug string) {
	config, _ := loadConfig()
	if config == nil {
		config = &Config{}
	}

	if slug != "" {
		// Direct model switch
		addModelToHistory(slug)
		fmt.Println(success("Switched to model: " + slug))
		return
	}

	// Interactive mode
	current := config.Model
	if current == "" {
		current = defaultModel
	}

	if len(config.ModelHistory) == 0 {
		fmt.Println(muted("No model history. Use: please -model <model-slug>"))
		return
	}

	selected := RunModelPicker("Select Model", config.ModelHistory, current)

	if selected == "" {
		fmt.Println(muted("Cancelled."))
		return
	}

	addModelToHistory(selected)
	fmt.Println(success("Switched to: " + selected))
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
	fmt.Println(errorStyle.Render("⚠ WARNING: This will completely wipe Please!"))
	fmt.Println()
	fmt.Println(muted("This will delete:"))
	fmt.Println(muted("  - All custom tools"))
	fmt.Println(muted("  - Your configuration (API key, model settings)"))
	fmt.Println(muted("  - All backups"))
	fmt.Println()
	fmt.Println("Press " + bold("1") + " to confirm, any other key to cancel...")

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println(errorf("Error: terminal not supported"))
		return
	}

	var buf [1]byte
	os.Stdin.Read(buf[:])
	term.Restore(int(os.Stdin.Fd()), oldState)

	if buf[0] != '1' {
		fmt.Println(muted("\nCancelled."))
		return
	}

	fmt.Println("1")
	fmt.Print("\nType " + bold("reset") + " and press Enter to confirm: ")

	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input != "reset" {
		fmt.Println(muted("Cancelled."))
		return
	}

	fmt.Println()
	fmt.Println(muted("Resetting..."))

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
		// Extract images from prompt and create appropriate message
		cleanedPrompt, imagePaths := extractImagesFromPrompt(prompt)
		if len(imagePaths) > 0 {
			fmt.Println(toolPrefixStyle.Render(fmt.Sprintf("> Attaching %d image(s)", len(imagePaths))))
		}
		messages = append(messages, createMultimodalMessage(cleanedPrompt, imagePaths))

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

		// Extract images from prompt
		cleanedPrompt, imagePaths := extractImagesFromPrompt(prompt)
		if len(imagePaths) > 0 {
			fmt.Println(toolPrefixStyle.Render(fmt.Sprintf("> Attaching %d image(s)", len(imagePaths))))
		}

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
			createMultimodalMessage(cleanedPrompt, imagePaths),
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
