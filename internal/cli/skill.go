package cli

import (
	"crypto/rand"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	ledgerlens "ledger-lens"
	"ledger-lens/internal/fault"
	"ledger-lens/internal/version"
)

type bundledFile struct {
	name    string
	content []byte
}

type exportResult struct {
	version.Info
	Files []string `json:"files"`
}

func readSkillFile(name string) ([]byte, error) {
	files, err := ledgerlens.SkillFiles()
	if err != nil {
		return nil, err
	}
	content, err := fs.ReadFile(files, name)
	if err != nil {
		return nil, fault.Invalid("请选择 SKILL.md、references/commands.md 或 agents/openai.yaml")
	}
	return content, nil
}

func promptContent() ([]byte, error) {
	metadata, err := readSkillFile("agents/openai.yaml")
	if err != nil {
		return nil, err
	}
	// This owned metadata uses one JSON-quoted YAML scalar. Keep the prompt in
	// that single source; unsupported editing formats fail instead of drifting.
	for _, line := range strings.Split(string(metadata), "\n") {
		if value, found := strings.CutPrefix(line, "  default_prompt: "); found {
			var prompt string
			if json.Unmarshal([]byte(value), &prompt) != nil || strings.TrimSpace(prompt) == "" {
				break
			}
			return []byte(strings.TrimRight(prompt, "\n") + "\n"), nil
		}
	}
	return nil, fault.New("BUNDLE_INVALID", "内置提示词无法读取，请重新安装 CLI", 1)
}

func exportSkill(dir string, force bool, info version.Info) (exportResult, error) {
	if strings.TrimSpace(dir) == "" {
		return exportResult{}, fault.Invalid("请通过 --dir 指定 Skill 导出目录")
	}
	files, err := ledgerlens.SkillFiles()
	if err != nil {
		return exportResult{}, err
	}
	var bundle []bundledFile
	err = fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		content, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		bundle = append(bundle, bundledFile{name: name, content: content})
		return nil
	})
	if err != nil {
		return exportResult{}, err
	}
	return exportBundle(dir, bundle, force, info)
}

func exportPrompt(output string, force bool, info version.Info) (exportResult, error) {
	if strings.TrimSpace(output) == "" {
		return exportResult{}, fault.Invalid("请通过 --output 指定提示词文件")
	}
	content, err := promptContent()
	if err != nil {
		return exportResult{}, err
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return exportResult{}, exportError(err)
	}
	return exportBundle(filepath.Dir(abs), []bundledFile{{name: filepath.Base(abs), content: content}}, force, info)
}

func exportBundle(dir string, files []bundledFile, force bool, info version.Info) (exportResult, error) {
	result := exportResult{Info: info, Files: []string{}}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return result, exportError(err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return result, exportError(err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return result, exportError(err)
	}
	defer root.Close()
	// Check every destination before writing, so an existing later file cannot
	// cause an ordinary collision to leave a partly exported Skill.
	for _, file := range files {
		if err := root.MkdirAll(filepath.Dir(file.name), 0o755); err != nil {
			return result, exportError(err)
		}
		stat, err := root.Lstat(file.name)
		if err != nil && !os.IsNotExist(err) {
			return result, exportError(err)
		}
		if err == nil {
			if !stat.Mode().IsRegular() {
				return result, fault.New("EXPORT_TARGET_INVALID", "导出位置含有非普通文件，请更换导出目录或文件名", 2)
			}
			if !force {
				return result, exportError(fs.ErrExist)
			}
		}
	}
	for _, file := range files {
		if err := writeExport(root, file, force); err != nil {
			return result, exportError(err)
		}
		result.Files = append(result.Files, filepath.Join(abs, file.name))
	}
	return result, nil
}

func writeExport(root *os.Root, file bundledFile, force bool) error {
	name := file.name
	if force {
		name = filepath.Join(filepath.Dir(name), ".ledgerlens-export-"+rand.Text())
		defer root.Remove(name)
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(file.content)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = root.Remove(name)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if force {
		return root.Rename(name, file.name)
	}
	return nil
}

func exportError(err error) error {
	if os.IsExist(err) {
		return fault.New("EXPORT_EXISTS", "目标文件已存在；如需覆盖，请添加 --force", 2)
	}
	return fault.New("EXPORT_FAILED", "导出未完成，请检查目标目录权限和剩余空间，或更换导出位置", 1)
}
