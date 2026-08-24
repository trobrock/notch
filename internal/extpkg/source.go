package extpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultGitHubAPI = "https://api.github.com"
	maxDownloadSize  = 128 << 20
)

var (
	githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	githubRepoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ParseSource parses a local directory, GitHub shorthand/URL, or generic Git
// source. ref and subdir override values encoded in the source when non-empty.
func ParseSource(raw, cwd, ref, subdir string) (Source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Source{}, errors.New("extension source is empty")
	}

	var source Source
	switch {
	case strings.HasPrefix(raw, "github:"):
		parsed, parsedRef, parsedSubdir, err := parseGitHubSpec(strings.TrimPrefix(raw, "github:"))
		if err != nil {
			return Source{}, err
		}
		source = Source{Type: "github", Location: parsed, Ref: parsedRef, Subdir: parsedSubdir}
	case strings.HasPrefix(raw, "local:"):
		path := strings.TrimPrefix(raw, "local:")
		absolute, err := absoluteLocalPath(path, cwd)
		if err != nil {
			return Source{}, err
		}
		source = Source{Type: "local", Location: absolute}
	case strings.HasPrefix(raw, "git:"):
		location, encodedRef := splitURLFragment(strings.TrimPrefix(raw, "git:"))
		if err := validateGitURL(location); err != nil {
			return Source{}, err
		}
		source = Source{Type: "git", Location: location, Ref: encodedRef}
	default:
		localCandidate := expandHome(raw)
		if !filepath.IsAbs(localCandidate) {
			localCandidate = filepath.Join(cwd, localCandidate)
		}
		if info, err := os.Stat(localCandidate); err == nil && info.IsDir() {
			absolute, err := absoluteLocalPath(raw, cwd)
			if err != nil {
				return Source{}, err
			}
			source = Source{Type: "local", Location: absolute}
			break
		}
		if parsed, ok, err := parseGitHubURL(raw); err != nil {
			return Source{}, err
		} else if ok {
			source = parsed
			break
		}
		location, encodedRef := splitURLFragment(raw)
		if err := validateGitURL(location); err != nil {
			return Source{}, fmt.Errorf("unsupported extension source %q: %w", raw, err)
		}
		source = Source{Type: "git", Location: location, Ref: encodedRef}
	}

	if ref != "" {
		if source.Ref != "" && source.Ref != ref {
			return Source{}, fmt.Errorf("source ref %q conflicts with --ref %q", source.Ref, ref)
		}
		source.Ref = ref
	}
	if subdir != "" {
		if source.Subdir != "" && filepath.Clean(source.Subdir) != filepath.Clean(filepath.FromSlash(subdir)) {
			return Source{}, fmt.Errorf("source subdirectory %q conflicts with --subdir %q", source.Subdir, subdir)
		}
		source.Subdir = subdir
	}
	if source.Subdir != "" {
		clean, err := cleanRelativePath(source.Subdir, false)
		if err != nil {
			return Source{}, fmt.Errorf("source subdirectory: %w", err)
		}
		source.Subdir = clean
	}
	if err := validateSource(source); err != nil {
		return Source{}, err
	}
	return source, nil
}

func validateSource(source Source) error {
	if source.Ref != "" && (strings.TrimSpace(source.Ref) != source.Ref || strings.HasPrefix(source.Ref, "-") || strings.ContainsAny(source.Ref, " \t\r\n\x00")) {
		return fmt.Errorf("invalid source ref %q", source.Ref)
	}
	if source.Subdir != "" {
		clean, err := cleanRelativePath(source.Subdir, false)
		if err != nil || clean != source.Subdir {
			return fmt.Errorf("invalid source subdirectory %q", source.Subdir)
		}
	}
	switch source.Type {
	case "github":
		location, _, _, err := parseGitHubSpec(source.Location)
		if err != nil || location != source.Location {
			return fmt.Errorf("invalid GitHub source location %q", source.Location)
		}
	case "git":
		if err := validateGitURL(source.Location); err != nil {
			return err
		}
	case "local":
		if !filepath.IsAbs(source.Location) || filepath.Clean(source.Location) != source.Location {
			return fmt.Errorf("local source %q must be an absolute clean path", source.Location)
		}
	default:
		return fmt.Errorf("unsupported source type %q", source.Type)
	}
	return nil
}

func parseGitHubSpec(spec string) (location, ref, subdir string, err error) {
	spec = strings.TrimSpace(spec)
	if index := strings.Index(spec, "//"); index >= 0 {
		subdir = spec[index+2:]
		spec = spec[:index]
	}
	if index := strings.LastIndex(spec, "@"); index > strings.LastIndex(spec, "/") {
		if index == len(spec)-1 {
			return "", "", "", errors.New("GitHub source ref is empty")
		}
		ref = spec[index+1:]
		spec = spec[:index]
	}
	parts := strings.Split(strings.TrimSuffix(spec, ".git"), "/")
	if len(parts) != 2 || !githubOwnerPattern.MatchString(parts[0]) || !githubRepoPattern.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
		return "", "", "", fmt.Errorf("invalid GitHub source %q (expected github:owner/repository)", spec)
	}
	return parts[0] + "/" + parts[1], ref, subdir, nil
}

func parseGitHubURL(raw string) (Source, bool, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() != "github.com" {
		return Source{}, false, nil
	}
	if parsed.User != nil {
		return Source{}, true, errors.New("GitHub source URL must not contain credentials; use GITHUB_TOKEN or GH_TOKEN")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Source{}, true, errors.New("GitHub URL must identify one repository; use --subdir for a package in a monorepo")
	}
	return Source{Type: "github", Location: parts[0] + "/" + parts[1], Ref: parsed.Fragment}, true, nil
}

func splitURLFragment(raw string) (string, string) {
	index := strings.LastIndex(raw, "#")
	if index < 0 {
		return raw, ""
	}
	return raw[:index], raw[index+1:]
}

func validateGitURL(raw string) error {
	if strings.TrimSpace(raw) == "" || strings.HasPrefix(raw, "-") {
		return errors.New("Git URL is empty or invalid")
	}
	if strings.HasPrefix(raw, "git@") {
		parts := strings.SplitN(strings.TrimPrefix(raw, "git@"), ":", 2)
		if len(parts) != 2 || parts[0] == "" || strings.HasPrefix(parts[0], "-") || parts[1] == "" {
			return errors.New("invalid SCP-style Git URL")
		}
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return errors.New("expected an existing local directory, github:owner/repository, or a Git URL")
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword || parsed.Scheme != "ssh" {
			return errors.New("Git URL must not contain credentials; use a credential helper or SSH username")
		}
	}
	switch parsed.Scheme {
	case "https", "ssh":
		if parsed.Hostname() == "" || strings.HasPrefix(parsed.Hostname(), "-") {
			return errors.New("Git URL host is empty or invalid")
		}
		return nil
	case "file":
		return nil
	case "http", "git":
		return fmt.Errorf("insecure Git URL scheme %q is not allowed", parsed.Scheme)
	default:
		return fmt.Errorf("unsupported Git URL scheme %q", parsed.Scheme)
	}
}

func absoluteLocalPath(path, cwd string) (string, error) {
	path = expandHome(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve local source %q: %w", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect local source %q: %w", absolute, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("local source %q is not a directory", absolute)
	}
	return filepath.Clean(absolute), nil
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"+string(filepath.Separator)))
		}
	}
	return path
}

func (s *Store) fetchSource(ctx context.Context, source Source, destination string) (string, error) {
	switch source.Type {
	case "local":
		root := source.Location
		if source.Subdir != "" {
			root = filepath.Join(root, source.Subdir)
		}
		if relative, err := filepath.Rel(root, destination); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", errors.New("local package source cannot contain the managed package staging directory")
		}
		if err := copyTree(root, destination); err != nil {
			return "", err
		}
		return localResolution(root)
	case "github":
		return s.fetchGitHub(ctx, source, destination)
	case "git":
		return s.fetchGit(ctx, source, destination)
	default:
		return "", fmt.Errorf("unsupported source type %q", source.Type)
	}
}

func (s *Store) fetchGitHub(ctx context.Context, source Source, destination string) (string, error) {
	ref := source.Ref
	if ref == "" {
		ref = "HEAD"
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	path := "/repos/" + source.Location + "/commits/" + url.PathEscape(ref)
	if err := s.githubJSON(ctx, path, &commit); err != nil {
		return "", fmt.Errorf("resolve GitHub ref %q: %w", ref, err)
	}
	if len(commit.SHA) < 7 {
		return "", errors.New("GitHub returned an invalid commit SHA")
	}
	archive, err := s.githubBytes(ctx, "/repos/"+source.Location+"/tarball/"+url.PathEscape(commit.SHA))
	if err != nil {
		return "", fmt.Errorf("download GitHub package: %w", err)
	}
	if err := extractGitHubTarGzipSubdir(archive, destination, source.Subdir); err != nil {
		return "", err
	}
	return commit.SHA, nil
}

func (s *Store) githubJSON(ctx context.Context, path string, target any) error {
	body, err := s.githubRequest(ctx, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

func (s *Store) githubBytes(ctx context.Context, path string) ([]byte, error) {
	return s.githubRequest(ctx, path)
}

func (s *Store) githubRequest(ctx context.Context, path string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.apiBaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "notch-extension-manager")
	if token := firstNonEmpty(os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	reader := io.LimitReader(response.Body, maxDownloadSize+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(body) > maxDownloadSize {
		return nil, fmt.Errorf("download exceeds %d MiB", maxDownloadSize>>20)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if len(message) > 512 {
			message = message[:512]
		}
		return nil, fmt.Errorf("GitHub returned %s: %s", response.Status, message)
	}
	return body, nil
}

func (s *Store) fetchGit(ctx context.Context, source Source, destination string) (string, error) {
	args := []string{"clone", "--quiet", "--depth", "1"}
	if source.Ref != "" {
		args = append(args, "--branch", source.Ref)
	}
	args = append(args, "--", source.Location, destination)
	if output, err := exec.CommandContext(ctx, s.gitPath, args...).CombinedOutput(); err != nil {
		if source.Ref == "" {
			return "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(output)))
		}
		_ = os.RemoveAll(destination)
		fallback := exec.CommandContext(ctx, s.gitPath, "clone", "--quiet", "--no-checkout", "--", source.Location, destination)
		if fallbackOutput, fallbackErr := fallback.CombinedOutput(); fallbackErr != nil {
			return "", fmt.Errorf("git clone: %w: %s", fallbackErr, strings.TrimSpace(string(output)+"\n"+string(fallbackOutput)))
		}
		fetch := exec.CommandContext(ctx, s.gitPath, "-C", destination, "fetch", "--quiet", "--depth", "1", "origin", source.Ref)
		if fetchOutput, fetchErr := fetch.CombinedOutput(); fetchErr != nil {
			return "", fmt.Errorf("git fetch ref %q: %w: %s", source.Ref, fetchErr, strings.TrimSpace(string(fetchOutput)))
		}
		checkout := exec.CommandContext(ctx, s.gitPath, "-C", destination, "checkout", "--quiet", "--detach", "FETCH_HEAD")
		if checkoutOutput, checkoutErr := checkout.CombinedOutput(); checkoutErr != nil {
			return "", fmt.Errorf("git checkout ref %q: %w: %s", source.Ref, checkoutErr, strings.TrimSpace(string(checkoutOutput)))
		}
	}
	resolvedBytes, err := exec.CommandContext(ctx, s.gitPath, "-C", destination, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("resolve cloned Git commit: %w", err)
	}
	resolved := strings.TrimSpace(string(resolvedBytes))
	if err := os.RemoveAll(filepath.Join(destination, ".git")); err != nil {
		return "", fmt.Errorf("remove package Git metadata: %w", err)
	}
	if source.Subdir != "" {
		selected := filepath.Join(destination, source.Subdir)
		temporary := destination + ".selected"
		if info, statErr := os.Stat(selected); statErr != nil || !info.IsDir() {
			if statErr != nil {
				return "", fmt.Errorf("package subdirectory %q: %w", source.Subdir, statErr)
			}
			return "", fmt.Errorf("package subdirectory %q is not a directory", source.Subdir)
		}
		if err := os.Rename(selected, temporary); err != nil {
			return "", fmt.Errorf("select package subdirectory: %w", err)
		}
		if err := os.RemoveAll(destination); err != nil {
			return "", err
		}
		if err := os.Rename(temporary, destination); err != nil {
			return "", err
		}
	}
	return resolved, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
