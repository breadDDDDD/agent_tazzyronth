// Package integrations holds the host-side files that "tazzy init" installs:
// the /tazzy command for each host (plan §7.1) and the Claude Code plugin.
// The *.json/*.toml snippets next to them show the MCP server entry for
// manual setup.
package integrations

import (
	"embed"
	"io/fs"
)

var (
	// Skill is an Agent Skills SKILL.md (name: tazzy), the current command
	// format of Codex, Cursor and Antigravity.
	//go:embed skill/tazzy/SKILL.md
	Skill string

	// GeminiCommand is a Gemini CLI custom command (.gemini/commands/tazzy.toml).
	//go:embed gemini/commands/tazzy.toml
	GeminiCommand string

	//go:embed claude-code/.claude-plugin/plugin.json claude-code/.claude-plugin/marketplace.json
	//go:embed claude-code/.mcp.json claude-code/commands claude-code/hooks claude-code/types
	claudePlugin embed.FS
)

// ClaudePlugin is the Claude Code plugin (also its own one-plugin
// marketplace), without its tests: the files "tazzy init" writes out.
func ClaudePlugin() fs.FS {
	sub, err := fs.Sub(claudePlugin, "claude-code")
	if err != nil {
		panic(err)
	}
	return sub
}
