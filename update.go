package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConnectorReleasesPage = "https://github.com/mihai209/Connector/releases"
	defaultConnectorRepoAPI      = "https://api.github.com/repos/mihai209/Connector/releases/latest"
)

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type githubRelease struct {
	TagName     string               `json:"tag_name"`
	Name        string               `json:"name"`
	HTMLURL     string               `json:"html_url"`
	PublishedAt string               `json:"published_at"`
	Assets      []githubReleaseAsset `json:"assets"`
}

type updaterState struct {
	ReleaseTag  string `json:"releaseTag"`
	ReleaseName string `json:"releaseName"`
	AssetURL    string `json:"assetUrl"`
	AssetName   string `json:"assetName"`
	InstalledAt string `json:"installedAt"`
}

var semverLikeRegex = regexp.MustCompile(`v?\d+\.\d+\.\d+(?:[-+._][0-9a-zA-Z.-]+)?`)

func runInteractiveSelfUpdate() error {
	fmt.Printf("Checking updates from %s ...\n", defaultConnectorReleasesPage)
	execPath, err := resolveExecutablePath()
	if err != nil {
		return err
	}

	release, err := fetchLatestGitHubRelease()
	if err != nil {
		return err
	}

	releaseLabel := firstNonEmpty(release.TagName, release.Name, "unknown")
	state := loadUpdaterState(execPath)
	if isReleaseAlreadyInstalled(state, release) {
		fmt.Printf("Already up to date (tracked by updater). current=%s latest=%s\n", ConnectorVersion, releaseLabel)
		return nil
	}

	latestComparable := extractComparableVersion(firstNonEmpty(release.TagName, release.Name))
	currentComparable := extractComparableVersion(ConnectorVersion)
	latestNormalized := normalizeVersionString(firstNonEmpty(release.TagName, release.Name))
	currentNormalized := normalizeVersionString(ConnectorVersion)
	if latestComparable != "" && currentComparable != "" {
		cmp := compareVersionStrings(currentComparable, latestComparable)
		if cmp > 0 {
			fmt.Printf("Already up to date. current=%s latest=%s\n", ConnectorVersion, releaseLabel)
			return nil
		}
		if cmp == 0 {
			// Numeric versions are equal. Keep "up to date" only when labels are effectively equivalent.
			if latestNormalized == "" || strings.EqualFold(latestNormalized, currentNormalized) || strings.HasPrefix(latestNormalized, currentNormalized+".") {
				fmt.Printf("Already up to date. current=%s latest=%s\n", ConnectorVersion, releaseLabel)
				return nil
			}
		}
	} else if latestNormalized != "" && strings.EqualFold(latestNormalized, currentNormalized) {
		fmt.Printf("Already up to date. current=%s latest=%s\n", ConnectorVersion, releaseLabel)
		return nil
	}

	fmt.Printf("Update available: current=%s latest=%s\n", ConnectorVersion, releaseLabel)
	fmt.Printf("Release page: %s\n", firstNonEmpty(release.HTMLURL, defaultConnectorReleasesPage))

	asset := selectBestAsset(release.Assets)
	if asset == nil {
		fmt.Println("No compatible release asset detected for this OS/ARCH.")
		fmt.Println("Open release page and update manually.")
		return nil
	}

	fmt.Printf("Detected asset: %s (%s)\n", asset.Name, formatBytes(asset.Size))
	yes, err := askYesNo("Do you want to install this version now? [y/N]: ")
	if err != nil {
		return err
	}
	if !yes {
		fmt.Println("Update skipped.")
		return nil
	}

	if err := installAssetUpdate(*asset, execPath); err != nil {
		return err
	}
	saveUpdaterState(execPath, updaterState{
		ReleaseTag:  strings.TrimSpace(release.TagName),
		ReleaseName: strings.TrimSpace(release.Name),
		AssetURL:    strings.TrimSpace(asset.BrowserDownloadURL),
		AssetName:   strings.TrimSpace(asset.Name),
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	})

	fmt.Println("Update installed successfully. Restart connector service to apply the new binary.")
	return nil
}

func fetchLatestGitHubRelease() (*githubRelease, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest(http.MethodGet, defaultConnectorRepoAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "cpanel-connector-go/"+ConnectorVersion)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github releases API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}
	return &release, nil
}

func selectBestAsset(assets []githubReleaseAsset) *githubReleaseAsset {
	if len(assets) == 0 {
		return nil
	}

	goos := strings.ToLower(runtime.GOOS)
	goarch := strings.ToLower(runtime.GOARCH)

	bestScore := -1
	bestIndex := -1
	for i := range assets {
		name := strings.ToLower(strings.TrimSpace(assets[i].Name))
		if name == "" || strings.Contains(name, ".sha256") || strings.Contains(name, ".sig") || strings.Contains(name, "checksums") {
			continue
		}

		score := 0
		if strings.Contains(name, "connector") {
			score += 2
		}
		if strings.Contains(name, "connector-go") {
			score += 3
		}
		if strings.Contains(name, goos) {
			score += 4
		}
		if strings.Contains(name, goarch) {
			score += 4
		}
		if strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			continue
		}
		if strings.HasSuffix(name, ".exe") && goos != "windows" {
			score -= 5
		}
		if goos == "windows" && !strings.HasSuffix(name, ".exe") {
			score -= 3
		}

		if score > bestScore {
			bestScore = score
			bestIndex = i
		}
	}

	if bestIndex < 0 || bestScore < 3 {
		return nil
	}
	return &assets[bestIndex]
}

func installAssetUpdate(asset githubReleaseAsset, execPath string) error {
	tmpFile, err := os.CreateTemp("", "connector-go-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest(http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "cpanel-connector-go/"+ConnectorVersion)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpPath, 0o755); err != nil {
			return err
		}
	}

	backupPath := execPath + ".bak"
	_ = os.Remove(backupPath)
	if err := os.Rename(execPath, backupPath); err != nil {
		return fmt.Errorf("cannot backup current binary (%s): %w", execPath, err)
	}

	if err := os.Rename(tmpPath, execPath); err != nil {
		_ = os.Rename(backupPath, execPath)
		return fmt.Errorf("cannot place updated binary: %w", err)
	}

	if runtime.GOOS != "windows" {
		_ = os.Chmod(execPath, 0o755)
	}

	fmt.Printf("Backup saved at: %s\n", backupPath)
	return nil
}

func resolveExecutablePath() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot detect executable path: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(execPath); resolveErr == nil && strings.TrimSpace(resolved) != "" {
		execPath = resolved
	}
	execPath = strings.TrimSpace(execPath)
	if execPath == "" {
		return "", fmt.Errorf("empty executable path")
	}
	return execPath, nil
}

func extractComparableVersion(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	match := semverLikeRegex.FindString(trimmed)
	if match == "" {
		return normalizeVersionString(trimmed)
	}
	return normalizeVersionString(match)
}

func updaterStatePath(execPath string) string {
	return execPath + ".release.json"
}

func loadUpdaterState(execPath string) updaterState {
	var state updaterState
	raw, err := os.ReadFile(updaterStatePath(execPath))
	if err != nil {
		return state
	}
	_ = json.Unmarshal(raw, &state)
	return state
}

func saveUpdaterState(execPath string, state updaterState) {
	path := updaterStatePath(execPath)
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o644)
}

func isReleaseAlreadyInstalled(state updaterState, release *githubRelease) bool {
	if release == nil {
		return false
	}
	remoteTag := strings.TrimSpace(release.TagName)
	remoteName := strings.TrimSpace(release.Name)
	if remoteTag != "" && strings.EqualFold(strings.TrimSpace(state.ReleaseTag), remoteTag) {
		return true
	}
	if remoteTag == "" && remoteName != "" && strings.EqualFold(strings.TrimSpace(state.ReleaseName), remoteName) {
		return true
	}
	return false
}

func askYesNo(prompt string) (bool, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func formatBytes(value int64) string {
	if value <= 0 {
		return "unknown size"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	precision := 0
	if unit > 1 {
		precision = 1
	}
	return strconv.FormatFloat(size, 'f', precision, 64) + " " + units[unit]
}
