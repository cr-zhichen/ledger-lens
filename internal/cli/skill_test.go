package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"ledger-lens/internal/update"
	"ledger-lens/internal/version"
)

func skillSources(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range []string{"SKILL.md", "references/commands.md", "agents/openai.yaml"} {
		content, err := os.ReadFile(filepath.Join("..", "..", ".agents", "skills", "ledgerlens", name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = content
	}
	return files
}

func contentCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	var checks atomic.Int32
	checker := checkerFunc(func(context.Context, string) (update.Result, error) {
		checks.Add(1)
		return update.Result{}, nil
	})
	db := filepath.Join(t.TempDir(), "must-not-exist", "ledger.sqlite")
	code := run(context.Background(), append([]string{"--db", db}, args...), strings.NewReader(""), &out, &stderr,
		version.Info{Version: "9.8.7", Commit: "bundle-commit", BuildDate: "2026-10-13T00:00:00Z"}, checker)
	if checks.Load() != 0 {
		t.Fatal("content command started an update request")
	}
	if _, err := os.Stat(filepath.Dir(db)); !os.IsNotExist(err) {
		t.Fatal("content command touched the account database directory")
	}
	if code != 0 && (out.Len() != 0 || !json.Valid(stderr.Bytes())) {
		t.Fatalf("broken error output: %s / %s", out.String(), stderr.String())
	}
	return code, out.String(), stderr.String()
}

func TestEmbeddedContentIndependentOfWorkingDirectory(t *testing.T) {
	t.Setenv("LEDGERLENS_NO_UPDATE_CHECK", "")
	sources := skillSources(t)
	t.Chdir(t.TempDir())
	for name, expected := range sources {
		// A sibling source file must not shadow the binary's own version.
		path := filepath.Join(".agents", "skills", "ledgerlens", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("outdated local content"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, stderr := contentCommand(t, "skill", "show", "--file", name)
		if code != 0 || out != string(expected) || stderr != "" {
			t.Fatalf("wrong embedded %s: %d / %s", name, code, stderr)
		}
	}
	code, prompt, stderr := contentCommand(t, "prompt", "show")
	if code != 0 || stderr != "" || strings.TrimSpace(prompt) == "" || !strings.HasSuffix(prompt, "\n") {
		t.Fatalf("invalid prompt: %d %q %s", code, prompt, stderr)
	}
	if !strings.Contains(string(sources["agents/openai.yaml"]), "default_prompt: "+strconv.Quote(strings.TrimSuffix(prompt, "\n"))) {
		t.Fatal("prompt does not match the distributed metadata")
	}
	code, out, stderr := contentCommand(t, "skill", "show")
	if code != 0 || out != string(sources["SKILL.md"]) {
		t.Fatalf("default skill view failed: %s", stderr)
	}
}

func TestSkillExportExactBundleAndExplicitReplacement(t *testing.T) {
	sources := skillSources(t)
	dir := filepath.Join(t.TempDir(), "ledgerlens")
	code, out, stderr := contentCommand(t, "skill", "export", "--dir", dir)
	if code != 0 {
		t.Fatal(stderr)
	}
	var result struct {
		Data exportResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Version != "9.8.7" || result.Data.Commit != "bundle-commit" || len(result.Data.Files) != len(sources) {
		t.Fatalf("missing bundle version/files: %s", out)
	}
	for _, path := range result.Data.Files {
		name, err := filepath.Rel(dir, path)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, sources[filepath.ToSlash(name)]) {
			t.Fatalf("export differed from source: %s / %v", path, err)
		}
	}
	modified := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(modified, []byte("local edits"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = contentCommand(t, "skill", "export", "--dir", dir)
	if code != 2 || !strings.Contains(stderr, "EXPORT_EXISTS") {
		t.Fatalf("existing file overwritten: %s", stderr)
	}
	if data, _ := os.ReadFile(modified); string(data) != "local edits" {
		t.Fatal("default export changed existing skill")
	}
	code, _, stderr = contentCommand(t, "skill", "export", "--dir", dir, "--force")
	if code != 0 {
		t.Fatal(stderr)
	}
	if data, _ := os.ReadFile(modified); !bytes.Equal(data, sources["SKILL.md"]) {
		t.Fatal("force did not update skill")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "keep.txt")); string(data) != "unrelated" {
		t.Fatal("force changed unrelated file")
	}
}

func TestExportPreflightAndSymlinkBoundaries(t *testing.T) {
	for _, mode := range []string{"existing", "directory", "symlink-file", "symlink-directory"} {
		t.Run(mode, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			references := filepath.Join(dir, "references")
			if err := os.Mkdir(references, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(references, "commands.md")
			sentinel := filepath.Join(outside, "commands.md")
			if err := os.WriteFile(sentinel, []byte("preserve me"), 0o644); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "existing":
				if err := os.WriteFile(target, []byte("local edits"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink-file":
				if err := os.Symlink(sentinel, target); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "symlink-directory":
				if err := os.Remove(references); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, references); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			args := []string{"skill", "export", "--dir", dir}
			if mode != "existing" {
				args = append(args, "--force")
			}
			code, _, stderr := contentCommand(t, args...)
			if code == 0 {
				t.Fatalf("invalid export succeeded: %s", mode)
			}
			if mode == "existing" && !strings.Contains(stderr, "EXPORT_EXISTS") {
				t.Fatalf("wrong collision error: %s", stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); !os.IsNotExist(err) {
				t.Fatal("preflight failure left a partially exported skill")
			}
			if data, _ := os.ReadFile(sentinel); string(data) != "preserve me" {
				t.Fatal("export modified a file outside its target directory")
			}
		})
	}
}

func TestPromptExportAndInvalidContentArguments(t *testing.T) {
	_, prompt, _ := contentCommand(t, "prompt", "show")
	t.Chdir(t.TempDir())
	code, _, stderr := contentCommand(t, "prompt", "export", "--output", "nested/prompt.txt")
	if code != 0 {
		t.Fatal(stderr)
	}
	if actual, err := os.ReadFile("nested/prompt.txt"); err != nil || string(actual) != prompt {
		t.Fatalf("prompt export differs: %v", err)
	}
	if err := os.WriteFile("nested/prompt.txt", []byte("old prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = contentCommand(t, "prompt", "export", "--output", "nested/prompt.txt")
	if code != 2 || !strings.Contains(stderr, "EXPORT_EXISTS") {
		t.Fatalf("prompt overwritten without force: %s", stderr)
	}
	code, _, stderr = contentCommand(t, "prompt", "export", "--output", "nested/prompt.txt", "--force")
	if code != 0 {
		t.Fatal(stderr)
	}
	if actual, _ := os.ReadFile("nested/prompt.txt"); string(actual) != prompt {
		t.Fatal("prompt was not replaced")
	}
	for _, args := range [][]string{
		{"skill", "show", "--file", "../../go.mod"}, {"skill", "show", "--file", "."},
		{"skill", "export"}, {"prompt", "export"}, {"prompt", "export", "--output", "."},
		{"skill", "unknown"}, {"prompt", "show", "--file", "SKILL.md"},
	} {
		code, _, stderr := contentCommand(t, args...)
		if code != 2 {
			t.Fatalf("invalid arguments %v: %d / %s", args, code, stderr)
		}
	}
}
