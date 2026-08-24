// Package upgrade installs verified Notch binaries from GitHub Releases.
package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	DefaultRepository = "trobrock/notch"
	defaultAPIBase    = "https://api.github.com"
	maxDownloadSize   = 256 << 20
	maxBinarySize     = 128 << 20
)

// Options controls release lookup and installation. APIBaseURL, Client,
// ExecutablePath, GOOS, and GOARCH primarily exist to keep the updater testable.
type Options struct {
	CurrentVersion string
	TargetVersion  string
	Repository     string
	APIBaseURL     string
	ExecutablePath string
	GOOS           string
	GOARCH         string
	CheckOnly      bool
	Force          bool
	Client         *http.Client
}

// Result describes the selected release and whether it was installed.
type Result struct {
	CurrentVersion string
	TargetVersion  string
	AssetName      string
	Available      bool
	Updated        bool
}

type release struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// Run checks GitHub Releases and optionally replaces the running executable.
func Run(ctx context.Context, options Options) (Result, error) {
	options = defaultOptions(options)
	selected, err := fetchRelease(ctx, options)
	if err != nil {
		return Result{}, err
	}
	if selected.Draft {
		return Result{}, fmt.Errorf("release %s is still a draft", selected.TagName)
	}
	targetSemver, err := parseSemVersion(selected.TagName)
	if err != nil {
		return Result{}, fmt.Errorf("release tag: %w", err)
	}
	result := Result{CurrentVersion: options.CurrentVersion, TargetVersion: selected.TagName}
	if currentSemver, currentErr := parseSemVersion(options.CurrentVersion); currentErr == nil {
		comparison := compareSemVersion(currentSemver, targetSemver)
		if comparison >= 0 && !options.Force {
			if comparison > 0 && options.TargetVersion != "" {
				return result, fmt.Errorf("target %s is older than current version %s (use --force to install it)", selected.TagName, options.CurrentVersion)
			}
			return result, nil
		}
	}
	result.Available = true
	assetName, err := archiveName(selected.TagName, options.GOOS, options.GOARCH)
	if err != nil {
		return result, err
	}
	result.AssetName = assetName
	assetURL, ok := findAsset(selected, assetName)
	if !ok {
		return result, fmt.Errorf("release %s does not contain %s", selected.TagName, assetName)
	}
	checksumsURL, ok := findAsset(selected, "checksums.txt")
	if !ok {
		return result, fmt.Errorf("release %s does not contain checksums.txt", selected.TagName)
	}
	if options.CheckOnly {
		return result, nil
	}

	checksums, err := download(ctx, options.Client, checksumsURL)
	if err != nil {
		return result, fmt.Errorf("download checksums: %w", err)
	}
	expected, err := checksumFor(checksums, assetName)
	if err != nil {
		return result, err
	}
	archive, err := download(ctx, options.Client, assetURL)
	if err != nil {
		return result, fmt.Errorf("download %s: %w", assetName, err)
	}
	actual := sha256.Sum256(archive)
	if !bytes.Equal(actual[:], expected) {
		return result, fmt.Errorf("checksum mismatch for %s", assetName)
	}
	binary, err := extractBinary(archive, options.GOOS)
	if err != nil {
		return result, fmt.Errorf("extract %s: %w", assetName, err)
	}
	executable := options.ExecutablePath
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return result, fmt.Errorf("locate current executable: %w", err)
		}
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	if err := replaceExecutable(executable, binary); err != nil {
		return result, fmt.Errorf("replace %s: %w", executable, err)
	}
	result.Updated = true
	return result, nil
}

func defaultOptions(options Options) Options {
	if options.Repository == "" {
		options.Repository = DefaultRepository
	}
	if options.APIBaseURL == "" {
		options.APIBaseURL = defaultAPIBase
	}
	options.APIBaseURL = strings.TrimRight(options.APIBaseURL, "/")
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	if options.GOARCH == "" {
		options.GOARCH = runtime.GOARCH
	}
	if options.Client == nil {
		options.Client = &http.Client{Timeout: 2 * time.Minute}
	}
	return options
}

func fetchRelease(ctx context.Context, options Options) (release, error) {
	path := "/repos/" + options.Repository + "/releases/latest"
	if options.TargetVersion != "" {
		tag := options.TargetVersion
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		if _, err := parseSemVersion(tag); err != nil {
			return release{}, err
		}
		path = "/repos/" + options.Repository + "/releases/tags/" + url.PathEscape(tag)
	}
	body, err := request(ctx, options.Client, options.APIBaseURL+path, "application/vnd.github+json")
	if err != nil {
		return release{}, fmt.Errorf("look up release: %w", err)
	}
	var selected release
	if err := json.Unmarshal(body, &selected); err != nil {
		return release{}, fmt.Errorf("decode release: %w", err)
	}
	if selected.TagName == "" {
		return release{}, errors.New("release response has no tag name")
	}
	return selected, nil
}

func findAsset(selected release, name string) (string, bool) {
	for _, asset := range selected.Assets {
		if asset.Name == name && asset.BrowserDownloadURL != "" {
			return asset.BrowserDownloadURL, true
		}
	}
	return "", false
}

func archiveName(tag, goos, goarch string) (string, error) {
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return "", fmt.Errorf("self-upgrade is not published for %s/%s", goos, goarch)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("self-upgrade is not published for %s/%s", goos, goarch)
	}
	version := strings.TrimPrefix(tag, "v")
	extension := ".tar.gz"
	if goos == "windows" {
		extension = ".zip"
	}
	return fmt.Sprintf("notch_%s_%s_%s%s", version, goos, goarch, extension), nil
}

func download(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	return request(ctx, client, rawURL, "application/octet-stream")
}

func request(ctx context.Context, client *http.Client, rawURL, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "notch-updater")
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(detail)))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDownloadSize {
		return nil, errors.New("download exceeds 256 MiB limit")
	}
	return body, nil
}

func checksumFor(contents []byte, assetName string) ([]byte, error) {
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != assetName {
			continue
		}
		checksum, err := hex.DecodeString(fields[0])
		if err != nil || len(checksum) != sha256.Size {
			return nil, fmt.Errorf("invalid checksum for %s", assetName)
		}
		return checksum, nil
	}
	return nil, fmt.Errorf("checksums.txt has no entry for %s", assetName)
}

func extractBinary(contents []byte, goos string) ([]byte, error) {
	binaryName := "notch"
	if goos == "windows" {
		binaryName += ".exe"
		reader, err := zip.NewReader(bytes.NewReader(contents), int64(len(contents)))
		if err != nil {
			return nil, err
		}
		for _, file := range reader.File {
			if file.Name != binaryName || !file.Mode().IsRegular() {
				continue
			}
			opened, err := file.Open()
			if err != nil {
				return nil, err
			}
			binary, readErr := readBinary(opened)
			closeErr := opened.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			return binary, nil
		}
		return nil, fmt.Errorf("archive has no regular %s", binaryName)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(contents))
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Name == binaryName && header.Typeflag == tar.TypeReg {
			return readBinary(tarReader)
		}
	}
	return nil, fmt.Errorf("archive has no regular %s", binaryName)
}

func readBinary(reader io.Reader) ([]byte, error) {
	binary, err := io.ReadAll(io.LimitReader(reader, maxBinarySize+1))
	if err != nil {
		return nil, err
	}
	if len(binary) > maxBinarySize {
		return nil, errors.New("binary exceeds 128 MiB limit")
	}
	if len(binary) == 0 {
		return nil, errors.New("binary is empty")
	}
	return binary, nil
}

func replaceExecutable(path string, binary []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("current executable is not a regular file")
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".notch-upgrade-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		_ = temporary.Close()
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := temporary.Write(binary); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		backup := path + ".old"
		_ = os.Remove(backup)
		if err := os.Rename(path, backup); err != nil {
			return err
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			_ = os.Rename(backup, path)
			return err
		}
		cleanup = false
		_ = os.Remove(backup)
		return nil
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	cleanup = false
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}
