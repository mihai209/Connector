package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const connectorVersionFeedURL = "https://cpanel-rocky.netlify.app/version-connector.json"

type connectorVersionFeed struct {
	Version       string `json:"version"`
	Latest        string `json:"latest"`
	LatestVersion string `json:"latest_version"`
	Stable        string `json:"stable"`
	Tag           string `json:"tag"`
}

func reportConnectorVersionStatus() {
	latest, err := fetchLatestConnectorVersion()
	if err != nil {
		bootWarn("version check unavailable: Rocky sa jucat cu codu din backend si la stricat si incearca sa il repare. details=%v", err)
		return
	}

	comparison := compareVersionStrings(ConnectorVersion, latest)
	switch {
	case comparison < 0:
		bootWarn("connector update available current=%s latest=%s", ConnectorVersion, latest)
	case comparison == 0:
		bootInfo("connector version up to date current=%s", ConnectorVersion)
	default:
		bootInfo("connector version ahead of feed current=%s latest=%s", ConnectorVersion, latest)
	}
}

func fetchLatestConnectorVersion() (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequest(http.MethodGet, connectorVersionFeedURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "cpanel-connector-go/"+ConnectorVersion)

	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected status %d", response.StatusCode)
	}

	var feed connectorVersionFeed
	if err := json.NewDecoder(response.Body).Decode(&feed); err != nil {
		return "", err
	}

	candidates := []string{feed.Latest, feed.LatestVersion, feed.Version, feed.Stable, feed.Tag}
	for _, candidate := range candidates {
		normalized := normalizeVersionString(candidate)
		if normalized != "" {
			return normalized, nil
		}
	}

	return "", fmt.Errorf("missing version fields in feed")
}

func compareVersionStrings(current, latest string) int {
	left := parseVersionNumbers(current)
	right := parseVersionNumbers(latest)

	maxLen := len(left)
	if len(right) > maxLen {
		maxLen = len(right)
	}

	for i := 0; i < maxLen; i++ {
		l := 0
		r := 0
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if l < r {
			return -1
		}
		if l > r {
			return 1
		}
	}
	return 0
}

func parseVersionNumbers(value string) []int {
	normalized := normalizeVersionString(value)
	if normalized == "" {
		return []int{0}
	}

	re := regexp.MustCompile(`\d+`)
	matches := re.FindAllString(normalized, -1)
	if len(matches) == 0 {
		return []int{0}
	}

	out := make([]int, 0, len(matches))
	for _, match := range matches {
		number, err := strconv.Atoi(match)
		if err != nil {
			continue
		}
		out = append(out, number)
	}
	if len(out) == 0 {
		return []int{0}
	}
	return out
}

func normalizeVersionString(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	trimmed = strings.TrimPrefix(trimmed, "v")
	return trimmed
}
