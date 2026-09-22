package update

import (
	"bytes"
	"compress/gzip"
	"debug/buildinfo"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"

	goupdate "github.com/inconshreveable/go-update"
	"github.com/rhysd/go-github-selfupdate/selfupdate"
	gitconfig "github.com/tcnksm/go-gitconfig"
)

const maxUpdateByteSize int64 = 256 << 20

func githubToken() string {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token
	}
	token, _ := gitconfig.GithubToken()
	return token
}

func updateRelease(release *selfupdate.Release, cmdPath string) error {
	return updateReleaseFromAPI(release, cmdPath, githubAPIBaseURL)
}

// 下载和验证全部完成后才替换程序，避免把 API JSON 或错误平台文件写入服务路径。
func updateReleaseFromAPI(release *selfupdate.Release, cmdPath, apiBase string) error {
	data, err := downloadRelease(release, apiBase)
	if err != nil {
		return err
	}
	if err := validateExecutable(data, runtime.GOOS, runtime.GOARCH); err != nil {
		return err
	}

	// 保存最近一次可执行文件，方便更新后的服务无法启动时人工恢复。
	if err := goupdate.Apply(bytes.NewReader(data), goupdate.Options{
		TargetPath:  cmdPath,
		OldSavePath: cmdPath + ".previous",
	}); err != nil {
		if rollbackErr := goupdate.RollbackError(err); rollbackErr != nil {
			return fmt.Errorf("failed to replace agent: %w; failed to restore previous executable: %v", err, rollbackErr)
		}
		return fmt.Errorf("failed to replace agent: %w", err)
	}
	return nil
}

func downloadRelease(release *selfupdate.Release, apiBase string) ([]byte, error) {
	if release == nil {
		return nil, fmt.Errorf("missing release")
	}
	expectedSize := int64(release.AssetByteSize)
	if expectedSize <= 0 || expectedSize > maxUpdateByteSize {
		return nil, fmt.Errorf("invalid release asset size %d (maximum %d)", expectedSize, maxUpdateByteSize)
	}
	if release.AssetID <= 0 || release.RepoOwner == "" || release.RepoName == "" {
		return nil, fmt.Errorf("missing release asset identity")
	}
	assetURL, err := url.Parse(release.AssetURL)
	if err != nil {
		return nil, fmt.Errorf("invalid release asset URL: %w", err)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(release.RepoOwner),
		url.PathEscape(release.RepoName),
		release.AssetID,
	)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create release asset request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "komari-agent")
	if token := githubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// 由同一个客户端跟随仓库改名和资源下载重定向，保留 Accept；
	// 避免旧 go-github 下载器在重定向后把 Accept 改成 */*。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download release asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release asset request returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, expectedSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read release asset: %w", err)
	}
	if int64(len(data)) != expectedSize {
		return nil, fmt.Errorf("release asset size mismatch: expected %d bytes, received %d", expectedSize, len(data))
	}

	if strings.HasSuffix(strings.ToLower(assetURL.Path), ".gz") {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("invalid gzip release asset: %w", err)
		}
		// 必须读到 EOF，使 gzip 同时验证 CRC；解压大小单独设限。
		uncompressed, readErr := io.ReadAll(io.LimitReader(reader, maxUpdateByteSize+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("failed to decompress release asset: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("failed to close gzip release asset: %w", closeErr)
		}
		if int64(len(uncompressed)) > maxUpdateByteSize {
			return nil, fmt.Errorf("uncompressed release asset exceeds %d bytes", maxUpdateByteSize)
		}
		data = uncompressed
	}
	return data, nil
}

func validateExecutable(data []byte, goos, goarch string) error {
	info, err := buildinfo.Read(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("release asset is not a valid Go executable: %w", err)
	}
	var actualOS, actualArch string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "GOOS":
			actualOS = setting.Value
		case "GOARCH":
			actualArch = setting.Value
		}
	}
	if actualOS != goos || actualArch != goarch {
		return fmt.Errorf("release executable platform mismatch: expected %s/%s, received %s/%s",
			goos, goarch, actualOS, actualArch)
	}
	return nil
}
