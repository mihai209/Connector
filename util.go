package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func runCommandWithInput(input string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func normalizeBrandHostname(name string) string {
	raw := strings.ToLower(strings.TrimSpace(name))
	if raw == "" {
		return "cpanel"
	}
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	if out == "" {
		return "cpanel"
	}
	return out
}

func parseHumanSizeToMB(raw string) (int, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "IB", "B")
	s = strings.ReplaceAll(s, "I", "")
	if s == "" {
		return 0, errors.New("empty size")
	}

	multiplier := 1.0
	switch {
	case strings.HasSuffix(s, "KB"):
		multiplier = 1.0 / 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "TB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "TB")
	case strings.HasSuffix(s, "B"):
		multiplier = 1.0 / (1024 * 1024)
		s = strings.TrimSuffix(s, "B")
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return int(math.Round(value * multiplier)), nil
}

func asString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	default:
		return ""
	}
}

func asInt(v interface{}) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case int32:
		return int(t)
	case float64:
		return int(t)
	case float32:
		return int(t)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(t))
		return i
	case json.Number:
		i, _ := strconv.Atoi(strings.TrimSpace(t.String()))
		return i
	default:
		return 0
	}
}

func asBool(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "1" || s == "yes" || s == "on"
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	default:
		return false
	}
}

func asMap(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func asSlice(v interface{}) []interface{} {
	if v == nil {
		return nil
	}
	if arr, ok := v.([]interface{}); ok {
		return arr
	}
	return nil
}

func asStringSlice(v interface{}) []string {
	arr := asSlice(v)
	if len(arr) == 0 {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		value := strings.TrimSpace(asString(item))
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func safeServerPath(volumesPath string, serverID int, rel string) (string, error) {
	serverRoot := filepath.Clean(filepath.Join(volumesPath, strconv.Itoa(serverID)))
	cleanRel := strings.ReplaceAll(rel, "\\", "/")
	if !strings.HasPrefix(cleanRel, "/") {
		cleanRel = "/" + cleanRel
	}
	cleanRel = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(cleanRel)), "/")
	full := filepath.Clean(filepath.Join(serverRoot, cleanRel))
	if full != serverRoot && !strings.HasPrefix(full, serverRoot+string(filepath.Separator)) {
		return "", errors.New("access denied: path escapes server directory")
	}
	return full, nil
}

func safeJoin(root, child string) (string, error) {
	full := filepath.Clean(filepath.Join(root, child))
	root = filepath.Clean(root)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", errors.New("access denied: path escapes root")
	}
	return full, nil
}

func resolveTemplateValue(raw string, context map[string]string) string {
	if raw == "" {
		return raw
	}
	result := raw
	for {
		start := strings.Index(result, "{{")
		if start < 0 {
			break
		}
		end := strings.Index(result[start+2:], "}}")
		if end < 0 {
			break
		}
		end = start + 2 + end
		placeholder := result[start : end+2]
		key := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(placeholder, "{{"), "}}"))
		replacement, ok := context[key]
		if !ok {
			replacement = placeholder
		}
		result = result[:start] + replacement + result[end+2:]
	}
	return result
}
