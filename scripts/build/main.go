// Build and package the CLI using the toolchain selected by mise.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ledger-lens/internal/version"
)

type target struct{ os, arch string }

var targets = []target{
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法：mise run build / package / release / release:validate")
	}
	mode := args[0]
	if mode != "local" && mode != "package" && mode != "release" && mode != "validate" {
		return fmt.Errorf("未知构建模式：%s", mode)
	}
	tag := os.Getenv("RELEASE_TAG")
	if (mode == "release" || mode == "validate") && tag == "" {
		return fmt.Errorf("请通过 RELEASE_TAG 指定已存在的正式版 Tag，例如 v1.2.3")
	}
	info, err := resolveVersion(".", tag)
	if err != nil {
		return err
	}
	if mode == "validate" {
		fmt.Fprintf(os.Stderr, "Tag %s 与 HEAD 一致，版本 %s\n", tag, info.Version)
		return nil
	}
	if mode == "local" {
		return build(filepath.Join("bin", executable(runtime.GOOS)), target{runtime.GOOS, runtime.GOARCH}, info)
	}
	return packageAll(info)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func resolveVersion(dir, tag string) (version.Info, error) {
	info := version.Info{Version: "dev", Commit: "unknown", BuildDate: time.Now().UTC().Format(time.RFC3339)}
	commit, err := git(dir, "rev-parse", "--verify", "HEAD")
	if err == nil {
		info.Commit = commit
	}
	status, statusErr := git(dir, "status", "--porcelain", "--untracked-files=normal")
	if tag != "" {
		value, valid := version.FromTag(tag)
		if !valid {
			return info, fmt.Errorf("RELEASE_TAG 必须是 vX.Y.Z 正式版，数字不能有前导零")
		}
		tagCommit, tagErr := git(dir, "rev-parse", "--verify", "refs/tags/"+tag+"^{commit}")
		if err != nil || tagErr != nil || tagCommit != commit {
			return info, fmt.Errorf("Tag %s 必须已存在且指向当前 HEAD", tag)
		}
		if statusErr != nil || status != "" {
			return info, fmt.Errorf("正式版构建要求工作区干净，请先提交代码")
		}
		info.Version = value
		return info, nil
	}
	if err != nil || statusErr != nil || status != "" {
		return info, nil
	}
	tags, err := git(dir, "tag", "--points-at", "HEAD")
	if err != nil {
		return info, nil
	}
	for _, candidate := range strings.Fields(tags) {
		value, valid := version.FromTag(candidate)
		if !valid {
			continue
		}
		cmp, comparable := version.Compare(value, info.Version)
		if !comparable || cmp > 0 {
			info.Version = value
		}
	}
	return info, nil
}

func executable(goos string) string {
	if goos == "windows" {
		return "ledgerlens.exe"
	}
	return "ledgerlens"
}

func build(path string, platform target, info version.Info) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	ldflags := fmt.Sprintf("-s -w -X ledger-lens/internal/version.Version=%s -X ledger-lens/internal/version.Commit=%s -X ledger-lens/internal/version.BuildDate=%s", info.Version, info.Commit, info.BuildDate)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", path, "./cmd/ledgerlens")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+platform.os, "GOARCH="+platform.arch)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	metadata, err := buildinfo.ReadFile(path)
	if err != nil {
		return err
	}
	settings := make(map[string]string)
	for _, setting := range metadata.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != platform.os || settings["GOARCH"] != platform.arch || settings["CGO_ENABLED"] != "0" {
		return fmt.Errorf("构建平台不匹配：%s", path)
	}
	if platform.os == runtime.GOOS && platform.arch == runtime.GOARCH {
		if err := smoke(path, info); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "已构建 %s（%s，%s/%s）\n", path, info.Version, platform.os, platform.arch)
	return nil
}

func smoke(path string, expected version.Info) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	out, err := exec.Command(abs, "--no-update-check", "version").Output()
	if err != nil {
		return fmt.Errorf("版本运行检查失败：%w", err)
	}
	var result struct {
		OK   bool         `json:"ok"`
		Data version.Info `json:"data"`
	}
	if err := json.Unmarshal(out, &result); err != nil || !result.OK || result.Data != expected {
		return fmt.Errorf("二进制中的版本信息与构建参数不一致：%s", path)
	}
	return nil
}

func packageAll(info version.Info) error {
	if err := os.MkdirAll("dist", 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp("dist", ".package-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	var assets []string
	var checksums strings.Builder
	for _, platform := range targets {
		name := fmt.Sprintf("ledgerlens_%s_%s_%s", info.Version, platform.os, platform.arch)
		binary := filepath.Join(stage, name, executable(platform.os))
		if err := build(binary, platform, info); err != nil {
			return err
		}
		extension := ".tar.gz"
		if platform.os == "windows" {
			extension = ".zip"
		}
		asset := name + extension
		path := filepath.Join(stage, asset)
		if err := archive(path, binary); err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&checksums, "%x  %s\n", sha256.Sum256(content), asset)
		assets = append(assets, asset)
	}
	if err := os.WriteFile(filepath.Join(stage, "SHA256SUMS"), []byte(checksums.String()), 0o644); err != nil {
		return err
	}
	assets = append(assets, "SHA256SUMS")
	for _, asset := range assets {
		if err := os.Rename(filepath.Join(stage, asset), filepath.Join("dist", asset)); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "已打包 %d 个平台及 SHA256SUMS，输出到 dist/\n", len(targets))
	return nil
}

type archiveFile struct {
	path, name string
	mode       int64
}

func archive(path, binary string) (err error) {
	files := []archiveFile{
		{binary, filepath.Base(binary), 0o755},
		{"README.md", "README.md", 0o644},
		{"LICENSE", "LICENSE", 0o644},
		{".agents/skills/ledgerlens/SKILL.md", ".agents/skills/ledgerlens/SKILL.md", 0o644},
		{".agents/skills/ledgerlens/references/commands.md", ".agents/skills/ledgerlens/references/commands.md", 0o644},
		{".agents/skills/ledgerlens/agents/openai.yaml", ".agents/skills/ledgerlens/agents/openai.yaml", 0o644},
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	if strings.HasSuffix(path, ".zip") {
		w := zip.NewWriter(f)
		for _, file := range files {
			info, err := os.Stat(file.path)
			if err != nil {
				return err
			}
			header := &zip.FileHeader{Name: file.name, Method: zip.Deflate, Modified: info.ModTime()}
			header.SetMode(os.FileMode(file.mode))
			entry, err := w.CreateHeader(header)
			if err != nil {
				return err
			}
			if err := copyFile(entry, file.path); err != nil {
				return err
			}
		}
		return w.Close()
	}
	gz := gzip.NewWriter(f)
	w := tar.NewWriter(gz)
	for _, file := range files {
		info, err := os.Stat(file.path)
		if err != nil {
			return err
		}
		if err := w.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: info.Size()}); err != nil {
			return err
		}
		if err := copyFile(w, file.path); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func copyFile(dst io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dst, f)
	return err
}
