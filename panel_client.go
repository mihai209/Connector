package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *Service) panelPostJSON(path string, payload interface{}, timeoutSec int) (map[string]interface{}, error) {
	baseURL, err := url.Parse(strings.TrimSpace(s.cfg.Panel.URL))
	if err != nil {
		return nil, err
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + path

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, baseURL.String(), bytes.NewReader(rawPayload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: sftpAuthTimeout}
	if timeoutSec > 0 {
		client.Timeout = time.Duration(timeoutSec) * time.Second
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	parsed := map[string]interface{}{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("invalid panel json response: %w", err)
		}
	}

	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(asString(parsed["error"]))
		if msg == "" {
			msg = fmt.Sprintf("panel returned status %d", resp.StatusCode)
		}
		return nil, fmt.Errorf(msg)
	}

	return parsed, nil
}
