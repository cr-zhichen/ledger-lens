package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func TestVersionComesOnlyFromCleanExactStableTag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	gitOK(t, dir, "init", "-b", "main")
	gitOK(t, dir, "config", "user.name", "Release test")
	gitOK(t, dir, "config", "user.email", "release@example.test")
	gitOK(t, dir, "config", "commit.gpgsign", "false")
	gitOK(t, dir, "config", "tag.gpgsign", "false")
	gitOK(t, dir, "config", "core.hooksPath", filepath.Join(dir, "disabled-hooks"))
	info, err := resolveVersion(dir, "")
	if err != nil || info.Version != "dev" || info.Commit != "unknown" {
		t.Fatalf("unborn repository mislabeled: %+v / %v", info, err)
	}
	gitOK(t, dir, "commit", "--allow-empty", "-m", "initial")
	gitOK(t, dir, "tag", "v1.9.0")
	gitOK(t, dir, "tag", "-a", "v1.10.0", "-m", "stable release")
	gitOK(t, dir, "tag", "v2.0.0-beta.1")
	for _, tag := range []string{"", "v1.10.0"} {
		info, err = resolveVersion(dir, tag)
		if err != nil || info.Version != "1.10.0" || info.Commit != gitOK(t, dir, "rev-parse", "HEAD") {
			t.Fatalf("stable tag was not injected: %+v / %v", info, err)
		}
	}
	for _, tag := range []string{"v2.0.0-beta.1", "v01.0.0", "v1.2.3", "1.10.0"} {
		if _, err := resolveVersion(dir, tag); err == nil {
			t.Errorf("invalid or missing release tag accepted: %s", tag)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = resolveVersion(dir, "")
	if err != nil || info.Version != "dev" {
		t.Fatal("dirty worktree was labeled as an official release")
	}
	if _, err := resolveVersion(dir, "v1.10.0"); err == nil {
		t.Fatal("dirty release build was accepted")
	}
	gitOK(t, dir, "add", "new.go")
	gitOK(t, dir, "commit", "-m", "next change")
	info, err = resolveVersion(dir, "")
	if err != nil || info.Version != "dev" {
		t.Fatal("post-tag commit inherited the release version")
	}
	if _, err := resolveVersion(dir, "v1.10.0"); err == nil {
		t.Fatal("release tag pointing to a different commit was accepted")
	}
}

func TestArchivesContainOnlyDistributableFilesAndExecutableMode(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"README.md", "LICENSE", ".agents/skills/ledgerlens/SKILL.md", ".agents/skills/ledgerlens/references/commands.md", ".agents/skills/ledgerlens/agents/openai.yaml", "data/private.sqlite", ".env"} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(path), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	for _, ext := range []string{".tar.gz", ".zip"} {
		binary := filepath.Join(dir, "ledgerlens")
		if ext == ".zip" {
			binary += ".exe"
		}
		if err := os.WriteFile(binary, []byte("binary fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "release"+ext)
		if err := archive(path, binary); err != nil {
			t.Fatal(err)
		}
		files := make(map[string]string)
		if ext == ".zip" {
			r, err := zip.OpenReader(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			for _, file := range r.File {
				if file.Modified.IsZero() || file.Modified.Year() < 1980 {
					t.Fatal("ZIP entry has an invalid DOS timestamp")
				}
				entry, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(entry)
				entry.Close()
				if err != nil {
					t.Fatal(err)
				}
				files[file.Name] = string(data)
				if file.Name == "ledgerlens.exe" && file.Mode().Perm() != 0o755 {
					t.Fatal("executable mode was lost")
				}
			}
		} else {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			gz, err := gzip.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			r := tar.NewReader(gz)
			for {
				header, err := r.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				files[header.Name] = string(data)
				if header.Name == "ledgerlens" && header.Mode != 0o755 {
					t.Fatal("executable mode was lost")
				}
			}
		}
		if len(files) != 6 || files[filepath.Base(binary)] != "binary fixture" || files[".agents/skills/ledgerlens/references/commands.md"] == "" || files["data/private.sqlite"] != "" || files[".env"] != "" {
			t.Fatalf("incorrect release contents: %v", files)
		}
	}
}
