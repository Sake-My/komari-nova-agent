package update

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/rhysd/go-github-selfupdate/selfupdate"
)

func testExecutable(t *testing.T) []byte {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateExecutable(data, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("test binary must contain platform build settings: %v", err)
	}
	return data
}

func gzipTestExecutable(t *testing.T, data []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	// 文件名只用于传输，实际平台必须由可执行文件内容确认。
	writer.Name = "different-local-service-name"
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func testDownloadRelease(data []byte, extension string) *selfupdate.Release {
	return &selfupdate.Release{
		AssetID:       123,
		AssetByteSize: len(data),
		AssetURL:      "https://github.com/new/agent/releases/download/1.2.62/agent" + extension,
		RepoOwner:     "old",
		RepoName:      "agent",
	}
}

func TestUpdateReleaseFollowsRenamedRepositoryAndPreservesBackup(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-only-token")
	binary := testExecutable(t)
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%t", compressed), func(t *testing.T) {
			payload, extension := binary, ""
			if compressed {
				payload, extension = gzipTestExecutable(t, binary), ".gz"
			}
			var paths []string
			var pathsMu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pathsMu.Lock()
				paths = append(paths, r.URL.Path)
				pathsMu.Unlock()
				if got := r.Header.Get("Accept"); got != "application/octet-stream" {
					t.Errorf("Accept = %q", got)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer test-only-token" {
					t.Errorf("Authorization was not preserved across same-origin redirects")
				}
				switch r.URL.Path {
				case "/repos/old/agent/releases/assets/123":
					http.Redirect(w, r, "/repos/new/agent/releases/assets/123", http.StatusMovedPermanently)
				case "/repos/new/agent/releases/assets/123":
					http.Redirect(w, r, "/binary", http.StatusFound)
				case "/binary":
					_, _ = w.Write(payload)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			path := filepath.Join(t.TempDir(), "agent")
			if err := os.WriteFile(path, []byte("current agent"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".previous", []byte("older backup"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := updateReleaseFromAPI(testDownloadRelease(payload, extension), path, server.URL); err != nil {
				t.Fatalf("update failed: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, binary) {
				t.Fatalf("updated program differs from validated binary: %v", err)
			}
			backup, err := os.ReadFile(path + ".previous")
			if err != nil || string(backup) != "current agent" {
				t.Fatalf("previous program was not saved: %v", err)
			}
			pathsMu.Lock()
			receivedPaths := strings.Join(paths, ",")
			pathsMu.Unlock()
			if receivedPaths != "/repos/old/agent/releases/assets/123,/repos/new/agent/releases/assets/123,/binary" {
				t.Fatalf("unexpected redirect chain: %s", receivedPaths)
			}
		})
	}
}

func replaceBuildSetting(t *testing.T, binary []byte, key, value string) []byte {
	t.Helper()
	before := []byte("build\t" + key + "=" + value + "\n")
	after := []byte("build\t" + key + "=" + strings.Repeat("x", len(value)) + "\n")
	if !bytes.Contains(binary, before) {
		t.Fatalf("test binary is missing %s build setting", key)
	}
	return bytes.ReplaceAll(binary, before, after)
}

func TestUpdateReleaseRejectsInvalidDownloadsWithoutChangingFiles(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-only-token")
	binary := testExecutable(t)
	badGzip := gzipTestExecutable(t, binary)
	badGzip[len(badGzip)-8] ^= 0xff // 修改 CRC，但保留完整压缩流和下载长度。

	tests := []struct {
		name      string
		payload   []byte
		size      int
		status    int
		extension string
	}{
		{"json", []byte("{\"id\":123,\"name\":\"agent\"}"), 0, http.StatusOK, ""},
		{"html", []byte("<html>download failed</html>"), 0, http.StatusOK, ""},
		{"truncated", binary[:len(binary)-1], len(binary), http.StatusOK, ""},
		{"oversized", binary, len(binary) - 1, http.StatusOK, ""},
		{"wrong-os", replaceBuildSetting(t, binary, "GOOS", runtime.GOOS), 0, http.StatusOK, ""},
		{"wrong-arch", replaceBuildSetting(t, binary, "GOARCH", runtime.GOARCH), 0, http.StatusOK, ""},
		{"http-error", binary, 0, http.StatusNotFound, ""},
		{"gzip-crc", badGzip, 0, http.StatusOK, ".gz"},
		{"gzip-json", gzipTestExecutable(t, []byte("{\"id\":123}")), 0, http.StatusOK, ".gz"},
		{"invalid-gzip", binary, 0, http.StatusOK, ".gz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write(tt.payload)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent")
			for name, content := range map[string]string{path: "current agent", path + ".previous": "previous backup"} {
				if err := os.WriteFile(name, []byte(content), 0755); err != nil {
					t.Fatal(err)
				}
			}
			release := testDownloadRelease(tt.payload, tt.extension)
			if tt.size != 0 {
				release.AssetByteSize = tt.size
			}
			if err := updateReleaseFromAPI(release, path, server.URL); err == nil {
				t.Fatal("invalid download unexpectedly succeeded")
			}
			for name, expected := range map[string]string{path: "current agent", path + ".previous": "previous backup"} {
				got, err := os.ReadFile(name)
				if err != nil || string(got) != expected {
					t.Fatalf("%s was changed after a failed download: %v", filepath.Base(name), err)
				}
			}
		})
	}
}

func TestDownloadReleaseRejectsInvalidSizeBeforeRequest(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-only-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid asset metadata should not start a download")
	}))
	defer server.Close()
	for _, size := range []int{0, -1, int(maxUpdateByteSize) + 1} {
		release := testDownloadRelease(nil, "")
		release.AssetByteSize = size
		if _, err := downloadRelease(release, server.URL); err == nil {
			t.Errorf("size %d was accepted", size)
		}
	}
}

func TestDownloadReleaseDoesNotForwardTokenToCDN(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-only-token")
	payload := []byte("release asset content")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("GitHub token was forwarded to another host")
		}
		if r.Header.Get("Accept") != "application/octet-stream" {
			t.Error("binary Accept header was lost during CDN redirect")
		}
		_, _ = w.Write(payload)
	}))
	defer cdn.Close()
	// 使用不同主机名模拟 API 到 CDN 的跳转，端口变化本身不算跨主机。
	cdnURL := strings.Replace(cdn.URL, "127.0.0.1", "localhost", 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only-token" {
			t.Error("GitHub API request is missing its token")
		}
		http.Redirect(w, r, cdnURL, http.StatusFound)
	}))
	defer api.Close()
	got, err := downloadRelease(testDownloadRelease(payload, ""), api.URL)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("CDN download failed: %v", err)
	}
}

func TestDownloadReleaseNetworkError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-only-token")
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	if _, err := downloadRelease(testDownloadRelease([]byte("data"), ""), server.URL); err == nil {
		t.Fatal("connection failure unexpectedly succeeded")
	}
}

func TestGitHubTokenUsesEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-only-token")
	if githubToken() != "test-only-token" {
		t.Fatal("GITHUB_TOKEN was not used")
	}
}
