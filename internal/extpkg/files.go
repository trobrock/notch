package extpkg

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	maxPackageFiles   = 10000
	maxPackageSize    = 512 << 20
	maxArchiveEntries = 100000
	maxArchiveSize    = 1 << 30
)

func extractGitHubTarGzip(contents []byte, destination string) error {
	return extractGitHubTarGzipSubdir(contents, destination, "")
}

func extractGitHubTarGzipSubdir(contents []byte, destination, subdir string) error {
	archiveSubdir := filepath.ToSlash(subdir)
	gzipReader, err := gzip.NewReader(bytes.NewReader(contents))
	if err != nil {
		return fmt.Errorf("open package archive: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return fmt.Errorf("create package staging directory: %w", err)
	}
	var root string
	seen := make(map[string]bool)
	files, total := 0, int64(0)
	archiveEntries, archiveTotal := 0, int64(0)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read package archive: %w", err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		clean := path.Clean(name)
		if clean == "." || clean == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("unsafe package archive path %q", header.Name)
		}
		parts := strings.Split(clean, "/")
		if root == "" {
			root = parts[0]
		}
		if parts[0] != root {
			return errors.New("package archive has multiple top-level roots")
		}
		archiveEntries++
		if archiveEntries > maxArchiveEntries {
			return fmt.Errorf("package archive contains more than %d entries", maxArchiveEntries)
		}
		if header.Size < 0 || header.Size > maxArchiveSize-archiveTotal {
			return fmt.Errorf("package archive expands beyond %d MiB", maxArchiveSize>>20)
		}
		archiveTotal += header.Size
		if len(parts) == 1 {
			if header.Typeflag != tar.TypeDir {
				return errors.New("package archive root is not a directory")
			}
			continue
		}
		relative := path.Join(parts[1:]...)
		if archiveSubdir != "" {
			if relative == archiveSubdir {
				if header.Typeflag != tar.TypeDir {
					return fmt.Errorf("package subdirectory %q is not a directory", subdir)
				}
				continue
			}
			prefix := archiveSubdir + "/"
			if !strings.HasPrefix(relative, prefix) {
				continue
			}
			relative = strings.TrimPrefix(relative, prefix)
		}
		if seen[relative] {
			return fmt.Errorf("duplicate package archive path %q", relative)
		}
		seen[relative] = true
		files++
		if files > maxPackageFiles {
			return fmt.Errorf("package contains more than %d entries", maxPackageFiles)
		}
		if header.Size < 0 || header.Size > maxPackageSize-total {
			return fmt.Errorf("package expands beyond %d MiB", maxPackageSize>>20)
		}
		total += header.Size
		target, err := archiveTarget(destination, relative)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, safeDirMode(header.FileInfo().Mode())); err != nil {
				return fmt.Errorf("create package directory %q: %w", relative, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, safeFileMode(header.FileInfo().Mode()))
			if err != nil {
				return fmt.Errorf("create package file %q: %w", relative, err)
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract package file %q: %w", relative, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close package file %q: %w", relative, closeErr)
			}
		default:
			return fmt.Errorf("package archive path %q uses unsupported type %d", relative, header.Typeflag)
		}
	}
	if root == "" || len(seen) == 0 {
		if subdir != "" {
			return fmt.Errorf("package subdirectory %q is empty or missing", subdir)
		}
		return errors.New("package archive is empty")
	}
	return nil
}

func archiveTarget(root, relative string) (string, error) {
	clean := path.Clean(relative)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe package archive path %q", relative)
	}
	platformPath := filepath.FromSlash(clean)
	if filepath.IsAbs(platformPath) || filepath.VolumeName(platformPath) != "" {
		return "", fmt.Errorf("unsafe package archive path %q", relative)
	}
	target := filepath.Join(root, platformPath)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("package archive path escapes destination: %q", relative)
	}
	return target, nil
}

func copyTree(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect package source %q: %w", source, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("package source %q is not a directory", source)
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	files, total := 0, int64(0)
	return filepath.WalkDir(source, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if entry.Name() == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		files++
		if files > maxPackageFiles {
			return fmt.Errorf("package contains more than %d entries", maxPackageFiles)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("package path %q is a symbolic link; links are not allowed", relative)
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		switch {
		case entryInfo.IsDir():
			return os.MkdirAll(target, safeDirMode(entryInfo.Mode()))
		case entryInfo.Mode().IsRegular():
			if entryInfo.Size() < 0 || entryInfo.Size() > maxPackageSize-total {
				return fmt.Errorf("package expands beyond %d MiB", maxPackageSize>>20)
			}
			total += entryInfo.Size()
			return copyFile(current, target, safeFileMode(entryInfo.Mode()))
		default:
			return fmt.Errorf("package path %q is not a regular file or directory", relative)
		}
	})
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func safeDirMode(mode os.FileMode) os.FileMode {
	mode = mode.Perm() & 0o755
	if mode&0o700 == 0 {
		mode |= 0o700
	}
	return mode
}

func safeFileMode(mode os.FileMode) os.FileMode {
	mode = mode.Perm() & 0o755
	if mode&0o600 == 0 {
		mode |= 0o600
	}
	return mode
}

func validatePackageTree(root string) error {
	files, total := 0, int64(0)
	return filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if current == root {
			return nil
		}
		files++
		if files > maxPackageFiles {
			return fmt.Errorf("package contains more than %d entries", maxPackageFiles)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsupported package path %q", current)
		}
		if info.Mode().IsRegular() {
			if info.Size() < 0 || info.Size() > maxPackageSize-total {
				return fmt.Errorf("package expands beyond %d MiB", maxPackageSize>>20)
			}
			total += info.Size()
		}
		return nil
	})
}

func treeIntegrity(root string) (string, error) {
	if err := validatePackageTree(root); err != nil {
		return "", err
	}
	var paths []string
	if err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if current == root {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		paths = append(paths, relative)
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(paths)
	hash := sha256.New()
	writer := bufio.NewWriter(hash)
	for _, relative := range paths {
		current := filepath.Join(root, relative)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return "", fmt.Errorf("unsupported installed package path %q", relative)
		}
		kind := "f"
		if info.IsDir() {
			kind = "d"
		}
		_, _ = writer.WriteString(filepath.ToSlash(relative))
		_ = writer.WriteByte(0)
		_, _ = writer.WriteString(kind)
		_ = writer.WriteByte(0)
		_, _ = writer.WriteString(strconv.FormatUint(uint64(info.Mode().Perm()), 8))
		_ = writer.WriteByte(0)
		if info.Mode().IsRegular() {
			file, err := os.Open(current)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(writer, file); err != nil {
				_ = file.Close()
				return "", err
			}
			if err := file.Close(); err != nil {
				return "", err
			}
		}
		_ = writer.WriteByte(0)
	}
	if err := writer.Flush(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func localResolution(string) (string, error) { return "local", nil }
