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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRunChecksAndInstallsVerifiedRelease(t *testing.T) {
	newBinary := []byte("new-notch-binary")
	archive := tarGzipBinary(t, newBinary)
	assetName := "notch_1.2.0_linux_amd64.tar.gz"
	digest := sha256.Sum256(archive)
	checksums := []byte(hex.EncodeToString(digest[:]) + "  " + assetName + "\n")
	var assetRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/trobrock/notch/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.0",
				"assets": []map[string]string{
					{"name": assetName, "browser_download_url": server.URL + "/asset"},
					{"name": "checksums.txt", "browser_download_url": server.URL + "/checksums"},
				},
			})
		case "/asset":
			assetRequests.Add(1)
			_, _ = w.Write(archive)
		case "/checksums":
			assetRequests.Add(1)
			_, _ = w.Write(checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	checked, err := Run(context.Background(), Options{
		CurrentVersion: "v1.1.0", APIBaseURL: server.URL,
		GOOS: "linux", GOARCH: "amd64", CheckOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !checked.Available || checked.Updated || checked.TargetVersion != "v1.2.0" || checked.AssetName != assetName {
		t.Fatalf("check result = %#v", checked)
	}
	if assetRequests.Load() != 0 {
		t.Fatalf("check downloaded %d assets", assetRequests.Load())
	}

	directory := t.TempDir()
	executable := filepath.Join(directory, "notch")
	if err := os.WriteFile(executable, []byte("old"), 0o751); err != nil {
		t.Fatal(err)
	}
	installed, err := Run(context.Background(), Options{
		CurrentVersion: "v1.1.0", APIBaseURL: server.URL, ExecutablePath: executable,
		GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !installed.Available || !installed.Updated {
		t.Fatalf("install result = %#v", installed)
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, newBinary) {
		t.Fatalf("installed contents = %q", contents)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("installed mode = %o", info.Mode().Perm())
	}
}

func TestRunRejectsChecksumMismatchWithoutReplacingExecutable(t *testing.T) {
	archive := tarGzipBinary(t, []byte("new"))
	assetName := "notch_2.0.0_linux_amd64.tar.gz"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/trobrock/notch/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v2.0.0",
				"assets": []map[string]string{
					{"name": assetName, "browser_download_url": server.URL + "/asset"},
					{"name": "checksums.txt", "browser_download_url": server.URL + "/checksums"},
				},
			})
		case "/asset":
			_, _ = w.Write(archive)
		case "/checksums":
			fmt.Fprintf(w, "%064d  %s\n", 0, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	executable := filepath.Join(t.TempDir(), "notch")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{
		CurrentVersion: "v1.0.0", APIBaseURL: server.URL, ExecutablePath: executable,
		GOOS: "linux", GOARCH: "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v", err)
	}
	contents, readErr := os.ReadFile(executable)
	if readErr != nil || string(contents) != "old" {
		t.Fatalf("executable changed: %q, %v", contents, readErr)
	}
}

func TestRunVersionSelection(t *testing.T) {
	releaseResponse := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/trobrock/notch/releases/tags/v1.5.0" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.5.0",
			"assets": []map[string]string{
				{"name": "notch_1.5.0_linux_amd64.tar.gz", "browser_download_url": "https://example.test/notch.tar.gz"},
				{"name": "checksums.txt", "browser_download_url": "https://example.test/checksums.txt"},
			},
		})
	}
	server := httptest.NewServer(http.HandlerFunc(releaseResponse))
	defer server.Close()

	result, err := Run(context.Background(), Options{
		CurrentVersion: "v1.5.0", TargetVersion: "1.5.0", APIBaseURL: server.URL,
		GOOS: "linux", GOARCH: "amd64", CheckOnly: true,
	})
	if err != nil || result.Available {
		t.Fatalf("current result = %#v, %v", result, err)
	}
	_, err = Run(context.Background(), Options{
		CurrentVersion: "v2.0.0", TargetVersion: "v1.5.0", APIBaseURL: server.URL,
		GOOS: "linux", GOARCH: "amd64", CheckOnly: true,
	})
	if err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("downgrade error = %v", err)
	}
	result, err = Run(context.Background(), Options{
		CurrentVersion: "dev", TargetVersion: "v1.5.0", APIBaseURL: server.URL,
		GOOS: "linux", GOARCH: "amd64", CheckOnly: true,
	})
	if err != nil || !result.Available {
		t.Fatalf("dev result = %#v, %v", result, err)
	}
}

func TestExtractWindowsBinary(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	header := &zip.FileHeader{Name: "notch.exe", Method: zip.Deflate}
	header.SetMode(0o755)
	file, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("windows-binary")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	binary, err := extractBinary(archive.Bytes(), "windows")
	if err != nil {
		t.Fatal(err)
	}
	if string(binary) != "windows-binary" {
		t.Fatalf("binary = %q", binary)
	}
}

func TestArchiveName(t *testing.T) {
	tests := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "notch_1.2.3_linux_amd64.tar.gz"},
		{"darwin", "arm64", "notch_1.2.3_darwin_arm64.tar.gz"},
		{"windows", "amd64", "notch_1.2.3_windows_amd64.zip"},
	}
	for _, test := range tests {
		got, err := archiveName("v1.2.3", test.goos, test.goarch)
		if err != nil || got != test.want {
			t.Errorf("archiveName(%s/%s) = %q, %v; want %q", test.goos, test.goarch, got, err, test.want)
		}
	}
	if _, err := archiveName("v1.2.3", "freebsd", "amd64"); err == nil {
		t.Fatal("unsupported platform succeeded")
	}
}

func tarGzipBinary(t *testing.T, binary []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "notch", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
