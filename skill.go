package ledgerlens

import (
	"embed"
	"io/fs"
)

// Embed the distributed sources directly so a build cannot carry an outdated
// generated copy. Explicit paths keep unrelated files out of the binary.
//
//go:embed .agents/skills/ledgerlens/SKILL.md .agents/skills/ledgerlens/references/commands.md .agents/skills/ledgerlens/agents/openai.yaml
var skillFiles embed.FS

func SkillFiles() (fs.FS, error) {
	return fs.Sub(skillFiles, ".agents/skills/ledgerlens")
}
