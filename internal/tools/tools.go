// Package tools implements the tools that are available without an extension.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	sharedprocess "github.com/trobrock/notch/internal/process"
)

const (
	// OutputLimit is the largest result returned by tools which can produce
	// unbounded output.
	OutputLimit      = 50 * 1024
	defaultReadLines = 2000
	maxLineBytes     = 1024 * 1024
	builtinSource    = "builtin"
)

var errOutputLimit = errors.New("tool output limit reached")

// RegisterBuiltins registers all built-in tools in reg.
func RegisterBuiltins(reg *extension.Registry, cwd string) error {
	if reg == nil {
		return errors.New("register built-in tools: nil registry")
	}
	for _, tool := range []extension.Tool{
		NewRead(cwd), NewWrite(cwd), NewEdit(cwd), NewBash(cwd),
		NewGrep(cwd), NewFind(cwd), NewLS(cwd),
	} {
		if err := reg.RegisterTool(tool); err != nil {
			return fmt.Errorf("register built-in tool %q: %w", tool.Definition.Name, err)
		}
	}
	return nil
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	s := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) != 0 {
		s["required"] = required
	}
	return s
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func definition(name, description string, schema map[string]any) model.ToolDefinition {
	return model.ToolDefinition{Name: name, Description: description, InputSchema: schema}
}

func decode(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("arguments are required")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	return nil
}

func resolvePath(cwd, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path must not be empty")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	base := cwd
	if base == "" {
		var err error
		base, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get current directory: %w", err)
		}
	} else if !filepath.IsAbs(base) {
		absolute, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("resolve working directory %q: %w", base, err)
		}
		base = absolute
	}
	return filepath.Clean(filepath.Join(base, path)), nil
}

func checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// NewRead constructs the read tool. Offset is one-based and limit counts lines.
func NewRead(cwd string) extension.Tool {
	return extension.Tool{
		Source: builtinSource,
		Definition: definition("read", "Read a text file, optionally selecting a range of lines.", objectSchema(map[string]any{
			"path":   stringProperty("File to read."),
			"offset": map[string]any{"type": "integer", "minimum": 1, "description": "One-based first line to read."},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "description": "Maximum number of lines to read."},
		}, "path")),
		Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			var args struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := decode(raw, &args); err != nil {
				return extension.ToolResult{}, err
			}
			if args.Offset == 0 {
				args.Offset = 1
			}
			if args.Limit == 0 {
				args.Limit = defaultReadLines
			}
			if args.Offset < 1 {
				return extension.ToolResult{}, errors.New("offset must be at least 1")
			}
			if args.Limit < 1 {
				return extension.ToolResult{}, errors.New("limit must be at least 1")
			}
			path, err := resolvePath(cwd, args.Path)
			if err != nil {
				return extension.ToolResult{}, err
			}
			file, err := os.Open(path)
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("read %q: %w", path, err)
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("stat %q: %w", path, err)
			}
			if !info.Mode().IsRegular() {
				return extension.ToolResult{}, fmt.Errorf("read %q: not a regular file", path)
			}

			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 32*1024), maxLineBytes)
			var out strings.Builder
			line, selected := 0, 0
			truncated := false
			for scanner.Scan() {
				if err := checkContext(ctx); err != nil {
					return extension.ToolResult{}, err
				}
				line++
				if line < args.Offset {
					continue
				}
				if selected == args.Limit {
					truncated = true
					break
				}
				text := scanner.Text()
				if !utf8.ValidString(text) {
					return extension.ToolResult{}, fmt.Errorf("read %q: file is not valid UTF-8 text", path)
				}
				separator := 0
				if selected > 0 {
					separator = 1
				}
				remaining := OutputLimit - out.Len()
				if separator+len(text) > remaining {
					if separator != 0 && remaining > 0 {
						out.WriteByte('\n')
						remaining--
					}
					if remaining > 0 {
						out.WriteString(validPrefix(text, remaining))
					}
					truncated = true
					break
				}
				if separator != 0 {
					out.WriteByte('\n')
				}
				out.WriteString(text)
				selected++
			}
			if err := scanner.Err(); err != nil {
				return extension.ToolResult{}, fmt.Errorf("read %q: %w", path, err)
			}
			result := extension.ToolResult{Content: out.String()}
			if truncated {
				result.Details = map[string]any{"truncated": true}
			}
			return result, nil
		},
	}
}

func validPrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}

// NewWrite constructs the write tool.
func NewWrite(cwd string) extension.Tool {
	return extension.Tool{
		Source: builtinSource,
		Definition: definition("write", "Write a file, creating parent directories when needed.", objectSchema(map[string]any{
			"path":    stringProperty("File to write."),
			"content": stringProperty("Complete file contents."),
		}, "path", "content")),
		Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			var args struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
			}
			if err := decode(raw, &args); err != nil {
				return extension.ToolResult{}, err
			}
			if args.Content == nil {
				return extension.ToolResult{}, errors.New("content is required")
			}
			if err := checkContext(ctx); err != nil {
				return extension.ToolResult{}, err
			}
			path, err := resolvePath(cwd, args.Path)
			if err != nil {
				return extension.ToolResult{}, err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return extension.ToolResult{}, fmt.Errorf("create parent directories for %q: %w", path, err)
			}
			if err := os.WriteFile(path, []byte(*args.Content), 0o644); err != nil {
				return extension.ToolResult{}, fmt.Errorf("write %q: %w", path, err)
			}
			return extension.ToolResult{Content: fmt.Sprintf("Wrote %d bytes to %s", len(*args.Content), args.Path)}, nil
		},
	}
}

// NewEdit constructs the exact replacement edit tool.
func NewEdit(cwd string) extension.Tool {
	return extension.Tool{
		Source: builtinSource,
		Definition: definition("edit", "Replace one exact, unique occurrence of text in a file.", objectSchema(map[string]any{
			"path":     stringProperty("File to edit."),
			"old_text": stringProperty("Exact text to replace; it must occur exactly once."),
			"new_text": stringProperty("Replacement text."),
		}, "path", "old_text", "new_text")),
		Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			var args struct {
				Path    string  `json:"path"`
				OldText string  `json:"old_text"`
				NewText *string `json:"new_text"`
			}
			if err := decode(raw, &args); err != nil {
				return extension.ToolResult{}, err
			}
			if args.OldText == "" {
				return extension.ToolResult{}, errors.New("old_text must not be empty")
			}
			if args.NewText == nil {
				return extension.ToolResult{}, errors.New("new_text is required")
			}
			if err := checkContext(ctx); err != nil {
				return extension.ToolResult{}, err
			}
			path, err := resolvePath(cwd, args.Path)
			if err != nil {
				return extension.ToolResult{}, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("read %q for edit: %w", path, err)
			}
			count := bytes.Count(content, []byte(args.OldText))
			if count == 0 {
				return extension.ToolResult{}, fmt.Errorf("edit %q: old_text was not found", path)
			}
			if count != 1 {
				return extension.ToolResult{}, fmt.Errorf("edit %q: old_text occurs %d times; it must be unique", path, count)
			}
			info, err := os.Stat(path)
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("stat %q: %w", path, err)
			}
			updated := bytes.Replace(content, []byte(args.OldText), []byte(*args.NewText), 1)
			if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
				return extension.ToolResult{}, fmt.Errorf("edit %q: %w", path, err)
			}
			return extension.ToolResult{Content: "Updated " + args.Path}, nil
		},
	}
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original != 0
		return original, nil
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.truncated = true
		return original, nil
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

// NewBash constructs the shell command tool.
func NewBash(cwd string) extension.Tool {
	return extension.Tool{
		Source: builtinSource,
		Definition: definition("bash", "Run a command with the system shell.", objectSchema(map[string]any{
			"command":    stringProperty("Shell command to execute."),
			"cwd":        stringProperty("Working directory. Relative paths are resolved against the configured working directory."),
			"timeout_ms": map[string]any{"type": "integer", "minimum": 1, "description": "Optional timeout in milliseconds."},
		}, "command")),
		Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			var args struct {
				Command   string `json:"command"`
				CWD       string `json:"cwd"`
				TimeoutMS int    `json:"timeout_ms"`
			}
			if err := decode(raw, &args); err != nil {
				return extension.ToolResult{}, err
			}
			if strings.TrimSpace(args.Command) == "" {
				return extension.ToolResult{}, errors.New("command must not be empty")
			}
			if args.TimeoutMS < 0 {
				return extension.ToolResult{}, errors.New("timeout_ms must not be negative")
			}
			workdir := "."
			if args.CWD != "" {
				workdir = args.CWD
			}
			resolvedCWD, err := resolvePath(cwd, workdir)
			if err != nil {
				return extension.ToolResult{}, err
			}
			info, err := os.Stat(resolvedCWD)
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("bash cwd %q: %w", resolvedCWD, err)
			}
			if !info.IsDir() {
				return extension.ToolResult{}, fmt.Errorf("bash cwd %q is not a directory", resolvedCWD)
			}

			runCtx := ctx
			cancel := func() {}
			if args.TimeoutMS > 0 {
				runCtx, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutMS)*time.Millisecond)
			}
			defer cancel()
			shell, shellArg := "/bin/sh", "-c"
			if runtime.GOOS == "windows" {
				shell, shellArg = "cmd.exe", "/C"
			}
			cmd := exec.CommandContext(runCtx, shell, shellArg, args.Command)
			cmd.Dir = resolvedCWD
			output := &cappedBuffer{limit: OutputLimit}
			cmd.Stdout, cmd.Stderr = output, output
			err = sharedprocess.RunCommand(runCtx, cmd)
			result := extension.ToolResult{Content: output.String()}
			details := map[string]any{}
			if output.truncated {
				details["truncated"] = true
			}
			if runCtx.Err() != nil {
				result.IsError = true
				result.Details = details
				return result, fmt.Errorf("bash command: %w", runCtx.Err())
			}
			if err == nil {
				if len(details) != 0 {
					result.Details = details
				}
				return result, nil
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				result.IsError = true
				details["exit_code"] = exitErr.ExitCode()
				result.Details = details
				return result, nil
			}
			return result, fmt.Errorf("run bash command: %w", err)
		},
	}
}

// NewGrep constructs the regular-expression search tool.
func NewGrep(cwd string) extension.Tool {
	return extension.Tool{
		Source: builtinSource,
		Definition: definition("grep", "Search files recursively using a Go regular expression.", objectSchema(map[string]any{
			"pattern": stringProperty("Go regular expression to search for."),
			"path":    stringProperty("File or directory to search (defaults to '.')."),
			"glob":    stringProperty("Optional filepath glob restricting searched files."),
		}, "pattern")),
		Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			var args struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Glob    string `json:"glob"`
			}
			if err := decode(raw, &args); err != nil {
				return extension.ToolResult{}, err
			}
			re, err := regexp.Compile(args.Pattern)
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("invalid grep pattern: %w", err)
			}
			if args.Glob != "" {
				if _, err := filepath.Match(args.Glob, "x"); err != nil {
					return extension.ToolResult{}, fmt.Errorf("invalid glob %q: %w", args.Glob, err)
				}
			}
			if args.Path == "" {
				args.Path = "."
			}
			root, err := resolvePath(cwd, args.Path)
			if err != nil {
				return extension.ToolResult{}, err
			}
			info, err := os.Stat(root)
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("grep path %q: %w", root, err)
			}
			var out cappedBuffer
			out.limit = OutputLimit
			search := func(path, display string) error {
				if err := checkContext(ctx); err != nil {
					return err
				}
				file, err := os.Open(path)
				if err != nil {
					return fmt.Errorf("grep open %q: %w", path, err)
				}
				defer file.Close()
				scanner := bufio.NewScanner(file)
				scanner.Buffer(make([]byte, 32*1024), maxLineBytes)
				line := 0
				for scanner.Scan() {
					if err := checkContext(ctx); err != nil {
						return err
					}
					line++
					text := scanner.Text()
					if re.MatchString(text) {
						entry := fmt.Sprintf("%s:%d:%s\n", display, line, text)
						_, _ = out.Write([]byte(entry))
						if out.truncated {
							return errOutputLimit
						}
					}
				}
				if err := scanner.Err(); err != nil {
					return fmt.Errorf("grep read %q: %w", path, err)
				}
				return nil
			}
			if info.Mode().IsRegular() {
				if args.Glob == "" || globMatches(args.Glob, filepath.Base(root), filepath.Base(root)) {
					err = search(root, filepath.Clean(args.Path))
				}
			} else if info.IsDir() {
				err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if err := checkContext(ctx); err != nil {
						return err
					}
					if entry.IsDir() {
						return nil
					}
					if entry.Type()&os.ModeSymlink != 0 {
						return nil
					}
					if !entry.Type().IsRegular() {
						info, e := entry.Info()
						if e != nil {
							return e
						}
						if !info.Mode().IsRegular() {
							return nil
						}
					}
					rel, e := filepath.Rel(root, path)
					if e != nil {
						return e
					}
					if args.Glob != "" && !globMatches(args.Glob, entry.Name(), rel) {
						return nil
					}
					return search(path, displayPath(args.Path, rel))
				})
			} else {
				return extension.ToolResult{}, fmt.Errorf("grep path %q is not a regular file or directory", root)
			}
			if err != nil && !errors.Is(err, errOutputLimit) {
				return extension.ToolResult{}, fmt.Errorf("grep %q: %w", root, err)
			}
			result := extension.ToolResult{Content: strings.TrimSuffix(out.String(), "\n")}
			if errors.Is(err, errOutputLimit) {
				result.Details = map[string]any{"truncated": true}
			}
			return result, nil
		},
	}
}

func globMatches(pattern, name, relative string) bool {
	matched, _ := filepath.Match(pattern, name)
	if matched {
		return true
	}
	matched, _ = filepath.Match(pattern, relative)
	return matched
}

func displayPath(input, relative string) string {
	if filepath.IsAbs(input) {
		return filepath.Join(filepath.Clean(input), relative)
	}
	if filepath.Clean(input) == "." {
		return filepath.Clean(relative)
	}
	return filepath.Join(filepath.Clean(input), relative)
}

// NewFind constructs the filename search tool.
func NewFind(cwd string) extension.Tool {
	return extension.Tool{
		Source: builtinSource,
		Definition: definition("find", "Find paths recursively by filepath glob.", objectSchema(map[string]any{
			"path":    stringProperty("Directory to search (defaults to '.')."),
			"pattern": stringProperty("Glob matched against each path's base name or relative path."),
		}, "pattern")),
		Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			var args struct {
				Path    string `json:"path"`
				Pattern string `json:"pattern"`
			}
			if err := decode(raw, &args); err != nil {
				return extension.ToolResult{}, err
			}
			if args.Pattern == "" {
				return extension.ToolResult{}, errors.New("pattern must not be empty")
			}
			if _, err := filepath.Match(args.Pattern, "x"); err != nil {
				return extension.ToolResult{}, fmt.Errorf("invalid find pattern %q: %w", args.Pattern, err)
			}
			if args.Path == "" {
				args.Path = "."
			}
			root, err := resolvePath(cwd, args.Path)
			if err != nil {
				return extension.ToolResult{}, err
			}
			info, err := os.Stat(root)
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("find path %q: %w", root, err)
			}
			if !info.IsDir() {
				return extension.ToolResult{}, fmt.Errorf("find path %q is not a directory", root)
			}
			out := &cappedBuffer{limit: OutputLimit}
			err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if err := checkContext(ctx); err != nil {
					return err
				}
				if path == root {
					return nil
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				if globMatches(args.Pattern, entry.Name(), rel) {
					_, _ = io.WriteString(out, displayPath(args.Path, rel)+"\n")
					if out.truncated {
						return errOutputLimit
					}
				}
				return nil
			})
			if err != nil && !errors.Is(err, errOutputLimit) {
				return extension.ToolResult{}, fmt.Errorf("find %q: %w", root, err)
			}
			result := extension.ToolResult{Content: strings.TrimSuffix(out.String(), "\n")}
			if errors.Is(err, errOutputLimit) {
				result.Details = map[string]any{"truncated": true}
			}
			return result, nil
		},
	}
}

// NewLS constructs the directory listing tool.
func NewLS(cwd string) extension.Tool {
	return extension.Tool{
		Source: builtinSource,
		Definition: definition("ls", "List a directory.", objectSchema(map[string]any{
			"path": stringProperty("Directory to list (defaults to '.')."),
		})),
		Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			var args struct {
				Path string `json:"path"`
			}
			if len(raw) == 0 {
				raw = json.RawMessage(`{}`)
			}
			if err := decode(raw, &args); err != nil {
				return extension.ToolResult{}, err
			}
			if err := checkContext(ctx); err != nil {
				return extension.ToolResult{}, err
			}
			if args.Path == "" {
				args.Path = "."
			}
			path, err := resolvePath(cwd, args.Path)
			if err != nil {
				return extension.ToolResult{}, err
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return extension.ToolResult{}, fmt.Errorf("list %q: %w", path, err)
			}
			// os.ReadDir sorts, but keep this explicit for alternate filesystems.
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			out := &cappedBuffer{limit: OutputLimit}
			for _, entry := range entries {
				if err := checkContext(ctx); err != nil {
					return extension.ToolResult{}, err
				}
				name := entry.Name()
				if entry.IsDir() {
					name += string(os.PathSeparator)
				}
				_, _ = io.WriteString(out, name+"\n")
				if out.truncated {
					break
				}
			}
			result := extension.ToolResult{Content: strings.TrimSuffix(out.String(), "\n")}
			if out.truncated {
				result.Details = map[string]any{"truncated": true}
			}
			return result, nil
		},
	}
}

// Tool-suffixed aliases keep constructor call sites self-documenting.
func NewReadTool(cwd string) extension.Tool  { return NewRead(cwd) }
func NewWriteTool(cwd string) extension.Tool { return NewWrite(cwd) }
func NewEditTool(cwd string) extension.Tool  { return NewEdit(cwd) }
func NewBashTool(cwd string) extension.Tool  { return NewBash(cwd) }
func NewGrepTool(cwd string) extension.Tool  { return NewGrep(cwd) }
func NewFindTool(cwd string) extension.Tool  { return NewFind(cwd) }
func NewLSTool(cwd string) extension.Tool    { return NewLS(cwd) }
