# please

A fast, agentic terminal assistant powered by AI. Just ask it to do things.

## Installation

### Homebrew (macOS/Linux)

```bash
brew tap Gabriel-Feang/tap
brew install please
```

### From source

```bash
go install github.com/Gabriel-Feang/please@latest
```

## Setup

Configure your OpenRouter API key:

```bash
please -setup
```

Get your API key at [openrouter.ai/keys](https://openrouter.ai/keys)

## Usage

### Basic usage

```bash
please list all large files in this directory
please what processes are using port 3000
please create a python script that fetches weather data
```

### Aliases

All of these work the same:
- `please` - full name
- `pls` - shortcut
- `plz` - shortcut

### Continue conversation with `hmm`

```bash
please show me my git status
hmm stage all the modified files
hmm now commit with a good message
```

The `hmm` command continues from your last `please` session (5 minute timeout).

### Interactive mode (for special characters)

If your prompt has quotes or special chars, just run without arguments:

```bash
pls
please> I wouldn't want to lose any files, can you help?
```

## Commands

| Command | Description |
|---------|-------------|
| `please -setup` | Configure your OpenRouter API key |
| `please -help` | Show help and available tools |
| `please -tools` | Toggle tools on/off interactively |
| `please -model [slug]` | Switch AI model (interactive if no slug) |
| `please -newtool <desc>` | Create a custom tool via AI |
| `please -rollback` | Restore from last backup |
| `please -reset` | Factory reset (wipes all config) |

## Tools

Built-in tools the agent can use:

| Tool | Description |
|------|-------------|
| `ask_user` | Ask user a question with options |
| `execute_command` | Run shell commands |
| `read_file` | Read file contents |
| `write_file` | Create/modify files |
| `list_files` | List directory contents |
| `grep_files` | Search with ripgrep |
| `tree` | Show directory structure |

### Custom Tools

Create your own tools with natural language:

```bash
please -newtool "a tool to fetch weather data for a city"
please -newtool "a tool to convert images to webp format"
```

The AI will write the tool implementation for you. Use `-rollback` if something goes wrong.

### Disabling Tools

Toggle tools on/off interactively:

```bash
please -tools
```

Disabled tools won't be available to the AI.

## Model Selection

Switch between AI models:

```bash
# Interactive selection from recent models
please -model

# Direct switch
please -model anthropic/claude-3-opus
please -model openai/gpt-4-turbo
```

The last 9 models you've used are saved for quick switching.

## Configuration

Config is stored at `~/.please/config.json`

```json
{
  "api_key": "sk-or-...",
  "model": "anthropic/claude-3.5-sonnet",
  "model_history": ["anthropic/claude-3-opus", "openai/gpt-4"],
  "disabled_tools": {}
}
```

You can also use the `OPENROUTER_API_KEY` environment variable.

## License

MIT
