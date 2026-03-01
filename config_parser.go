package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type ConfigFileDefinition struct {
	Parser       string
	Replacements []ConfigReplaceEntry
}

type ConfigReplaceEntry struct {
	Match       string
	ReplaceWith interface{}
	HasIfValue  bool
	IfValue     interface{}
}

func (s *Service) applyEggConfigFiles(serverID int, serverPath string, cfg ServerInstallConfig) error {
	rawDefinitions := resolveRawConfigFiles(cfg)
	definitions := normalizeConfigFileDefinitions(rawDefinitions)
	if len(definitions) == 0 {
		context := buildEggTemplateContext(cfg)
		if err := applyFallbackServerPropertiesPort(serverPath, context, definitions); err != nil {
			return err
		}
		return nil
	}

	context := buildEggTemplateContext(cfg)
	s.sendConsoleOutput(serverID, "\x1b[1;34m[*] Applying egg config file parsers...\x1b[0m\n")

	for relativePath, definition := range definitions {
		targetPath, err := safeJoin(serverPath, strings.TrimPrefix(filepath.FromSlash(relativePath), string(filepath.Separator)))
		if err != nil {
			return fmt.Errorf("invalid config file path %s: %w", relativePath, err)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}

		content := ""
		if raw, err := os.ReadFile(targetPath); err == nil {
			content = string(raw)
		} else if definition.Parser == "json" || definition.Parser == "yaml" || definition.Parser == "yml" {
			content = "{}\n"
		}

		finalContent, err := applyConfigParser(definition.Parser, content, definition.Replacements, context)
		if err != nil {
			return fmt.Errorf("%s parser error: %w", relativePath, err)
		}

		if err := os.WriteFile(targetPath, []byte(finalContent), 0o644); err != nil {
			return err
		}
		s.sendConsoleOutput(serverID, fmt.Sprintf("\x1b[1;34m[*] Patched %s (%s).\x1b[0m\n", relativePath, definition.Parser))
	}

	if err := applyFallbackServerPropertiesPort(serverPath, context, definitions); err != nil {
		return err
	}

	s.sendConsoleOutput(serverID, "\x1b[1;32m[✓] Egg config files applied.\x1b[0m\n")
	return nil
}

func applyFallbackServerPropertiesPort(serverPath string, context map[string]string, definitions map[string]ConfigFileDefinition) error {
	port := strings.TrimSpace(context["SERVER_PORT"])
	if port == "" {
		return nil
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return nil
	}

	shouldPatch := false
	for key := range definitions {
		normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "\\", "/"))
		if normalized == "server.properties" || strings.HasSuffix(normalized, "/server.properties") {
			shouldPatch = true
			break
		}
	}

	targetPath, joinErr := safeJoin(serverPath, "server.properties")
	if joinErr != nil {
		return fmt.Errorf("invalid server.properties path: %w", joinErr)
	}

	raw, readErr := os.ReadFile(targetPath)
	if readErr != nil {
		if !shouldPatch {
			return nil
		}
		raw = []byte{}
	}

	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "server-port=") || strings.HasPrefix(lower, "server-port:") {
			lines[i] = "server-port=" + strconv.Itoa(parsedPort)
			found = true
		}
	}
	if !found {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "server-port="+strconv.Itoa(parsedPort))
	}

	updated := strings.Join(lines, "\n")
	if !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}

	return os.WriteFile(targetPath, []byte(updated), 0o644)
}

func resolveRawConfigFiles(cfg ServerInstallConfig) interface{} {
	if len(cfg.EggConfig) > 0 {
		if raw := cfg.EggConfig["files"]; raw != nil {
			return raw
		}
	}
	return cfg.ConfigFiles
}

func buildEggTemplateContext(cfg ServerInstallConfig) map[string]string {
	context := map[string]string{}
	for key, value := range cfg.Env {
		str := stringifyEnvValue(value)
		context[key] = str
		context["env."+key] = str
		context["server.build.env."+key] = str
		context["server.environment."+key] = str
	}

	port := ""
	ip := "0.0.0.0"
	if len(cfg.Ports) > 0 {
		port = strconv.Itoa(cfg.Ports[0].Host)
	}
	if v, ok := context["SERVER_PORT"]; ok && strings.TrimSpace(v) != "" {
		port = v
	}
	if v, ok := context["SERVER_IP"]; ok && strings.TrimSpace(v) != "" {
		ip = v
	}

	context["server.build.default.ip"] = ip
	context["server.build.default.port"] = port
	context["server.build.default.port_raw"] = port
	context["server.build.memory_limit"] = strconv.Itoa(cfg.Memory)
	context["server.build.cpu_limit"] = strconv.Itoa(cfg.CPU)
	context["server.build.disk"] = strconv.Itoa(cfg.Disk)
	context["server.build.startup"] = cfg.Startup

	return context
}

func normalizeConfigFileDefinitions(raw interface{}) map[string]ConfigFileDefinition {
	result := map[string]ConfigFileDefinition{}
	if raw == nil {
		return result
	}

	payload := raw
	if payloadStr, ok := payload.(string); ok {
		payloadStr = strings.TrimSpace(payloadStr)
		if payloadStr == "" {
			return result
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(payloadStr), &parsed); err != nil {
			return result
		}
		payload = parsed
	}

	consumeDefinition := func(fileName string, rawDef interface{}) {
		fileName = strings.TrimSpace(fileName)
		if fileName == "" {
			return
		}
		definition := normalizeSingleDefinition(rawDef)
		if definition.Parser == "" || len(definition.Replacements) == 0 {
			return
		}
		result[fileName] = definition
	}

	if m := asMap(payload); m != nil {
		if file, ok := m["file"]; ok {
			fileName := strings.TrimSpace(asString(file))
			if fileName != "" {
				consumeDefinition(fileName, m)
				return result
			}
		}
		for fileName, def := range m {
			consumeDefinition(fileName, def)
		}
		return result
	}

	if arr := asSlice(payload); len(arr) > 0 {
		for _, item := range arr {
			entry := asMap(item)
			if entry == nil {
				continue
			}
			fileName := strings.TrimSpace(asString(entry["file"]))
			if fileName == "" {
				fileName = strings.TrimSpace(asString(entry["fileName"]))
			}
			if fileName == "" {
				fileName = strings.TrimSpace(asString(entry["filename"]))
			}
			if fileName == "" {
				fileName = strings.TrimSpace(asString(entry["path"]))
			}
			consumeDefinition(fileName, entry)
		}
	}

	return result
}

func normalizeSingleDefinition(raw interface{}) ConfigFileDefinition {
	defMap := asMap(raw)
	if defMap == nil {
		if rawStr, ok := raw.(string); ok {
			var parsed interface{}
			if json.Unmarshal([]byte(rawStr), &parsed) == nil {
				defMap = asMap(parsed)
			}
		}
	}
	if defMap == nil {
		return ConfigFileDefinition{}
	}

	parser := strings.ToLower(strings.TrimSpace(asString(defMap["parser"])))
	if parser == "" {
		parser = strings.ToLower(strings.TrimSpace(asString(defMap["format"])))
	}
	if parser == "" {
		parser = "file"
	}

	replacements := make([]ConfigReplaceEntry, 0)
	if find := defMap["find"]; find != nil {
		if findMap := asMap(find); findMap != nil {
			for k, v := range findMap {
				replacements = append(replacements, ConfigReplaceEntry{Match: strings.TrimSpace(k), ReplaceWith: v})
			}
		}
	}
	if repl := defMap["replacements"]; repl != nil {
		if replMap := asMap(repl); replMap != nil {
			for k, v := range replMap {
				replacements = append(replacements, ConfigReplaceEntry{Match: strings.TrimSpace(k), ReplaceWith: v})
			}
		}
	}

	if replaceItems := asSlice(defMap["replace"]); len(replaceItems) > 0 {
		for _, item := range replaceItems {
			entry := asMap(item)
			if entry == nil {
				continue
			}
			match := strings.TrimSpace(asString(entry["match"]))
			if match == "" {
				continue
			}
			repl := ConfigReplaceEntry{Match: match}
			if v, ok := entry["replace_with"]; ok {
				repl.ReplaceWith = v
			} else {
				repl.ReplaceWith = entry["replaceWith"]
			}
			if v, ok := entry["if_value"]; ok {
				repl.HasIfValue = true
				repl.IfValue = v
			} else if v, ok := entry["ifValue"]; ok {
				repl.HasIfValue = true
				repl.IfValue = v
			}
			replacements = append(replacements, repl)
		}
	}

	// normalize replacements
	normalized := make([]ConfigReplaceEntry, 0, len(replacements))
	for _, entry := range replacements {
		if strings.TrimSpace(entry.Match) == "" {
			continue
		}
		normalized = append(normalized, entry)
	}

	return ConfigFileDefinition{Parser: parser, Replacements: normalized}
}

func applyConfigParser(parser string, content string, replacements []ConfigReplaceEntry, context map[string]string) (string, error) {
	resolved := resolveConfigReplacements(replacements, context)
	if len(resolved) == 0 {
		if content == "" {
			return "", nil
		}
		if strings.HasSuffix(content, "\n") {
			return content, nil
		}
		return content + "\n", nil
	}

	switch strings.ToLower(strings.TrimSpace(parser)) {
	case "json":
		return applyJSONParser(content, resolved)
	case "yaml", "yml":
		return applyYAMLParser(content, resolved)
	case "properties":
		return applyKVParser(content, resolved, false), nil
	case "ini":
		return applyKVParser(content, resolved, true), nil
	case "file":
		return applyFileParser(content, resolved), nil
	default:
		return applyTextParser(content, resolved), nil
	}
}

func resolveConfigReplacements(entries []ConfigReplaceEntry, context map[string]string) []ConfigReplaceEntry {
	resolved := make([]ConfigReplaceEntry, 0, len(entries))
	for _, entry := range entries {
		match := strings.TrimSpace(resolveTemplateValue(entry.Match, context))
		if match == "" {
			continue
		}
		item := ConfigReplaceEntry{
			Match:       match,
			ReplaceWith: resolveTemplateAny(entry.ReplaceWith, context),
			HasIfValue:  entry.HasIfValue,
		}
		if entry.HasIfValue {
			item.IfValue = resolveTemplateAny(entry.IfValue, context)
		}
		resolved = append(resolved, item)
	}
	return resolved
}

func resolveTemplateAny(value interface{}, context map[string]string) interface{} {
	if value == nil {
		return ""
	}
	if str, ok := value.(string); ok {
		return resolveTemplateValue(str, context)
	}
	return value
}

func applyJSONParser(content string, entries []ConfigReplaceEntry) (string, error) {
	var payload interface{}
	if strings.TrimSpace(content) == "" {
		payload = map[string]interface{}{}
	} else if err := json.Unmarshal([]byte(content), &payload); err != nil {
		payload = map[string]interface{}{}
	}

	if payload == nil {
		payload = map[string]interface{}{}
	}
	applyStructuredReplacements(&payload, entries)

	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw) + "\n", nil
}

func applyYAMLParser(content string, entries []ConfigReplaceEntry) (string, error) {
	var payload interface{}
	if strings.TrimSpace(content) == "" {
		payload = map[string]interface{}{}
	} else if err := yaml.Unmarshal([]byte(content), &payload); err != nil {
		payload = map[string]interface{}{}
	}

	payload = normalizeYAMLValue(payload)
	applyStructuredReplacements(&payload, entries)

	raw, err := yaml.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func normalizeYAMLValue(value interface{}) interface{} {
	switch t := value.(type) {
	case map[interface{}]interface{}:
		mapped := map[string]interface{}{}
		for k, v := range t {
			mapped[asString(k)] = normalizeYAMLValue(v)
		}
		return mapped
	case map[string]interface{}:
		mapped := map[string]interface{}{}
		for k, v := range t {
			mapped[k] = normalizeYAMLValue(v)
		}
		return mapped
	case []interface{}:
		arr := make([]interface{}, len(t))
		for i := range t {
			arr[i] = normalizeYAMLValue(t[i])
		}
		return arr
	default:
		return value
	}
}

func applyKVParser(content string, entries []ConfigReplaceEntry, sectioned bool) string {
	lines := []string{}
	if content != "" {
		lines = strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	}

	target := map[string]ConfigReplaceEntry{}
	for _, entry := range entries {
		if strings.Contains(entry.Match, "*") || strings.Contains(entry.Match, "[") || strings.Contains(entry.Match, "]") {
			continue
		}
		target[entry.Match] = entry
	}

	currentSection := ""
	updated := map[string]bool{}
	reKV := regexp.MustCompile(`^\s*([A-Za-z0-9_.-]+)\s*[:=]\s*(.*?)\s*$`)
	reSection := regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)

	for i, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if sectioned {
			if matches := reSection.FindStringSubmatch(trimmed); len(matches) == 2 {
				currentSection = strings.TrimSpace(matches[1])
				continue
			}
		}

		matches := reKV.FindStringSubmatch(rawLine)
		if len(matches) != 3 {
			continue
		}
		key := matches[1]
		fullKey := key
		if sectioned && currentSection != "" {
			fullKey = currentSection + "." + key
		}

		entry, ok := target[fullKey]
		if !ok {
			entry, ok = target[key]
		}
		if !ok {
			continue
		}

		if entry.HasIfValue {
			if !valuesEquivalent(strings.TrimSpace(matches[2]), entry.IfValue) {
				continue
			}
		}

		lines[i] = fmt.Sprintf("%s=%s", key, stringifyScalar(entry.ReplaceWith))
		updated[entry.Match] = true
	}

	for key, entry := range target {
		if updated[key] || entry.HasIfValue {
			continue
		}
		if sectioned && strings.Contains(key, ".") {
			parts := strings.SplitN(key, ".", 2)
			sectionName := parts[0]
			sectionKey := parts[1]
			if !hasSection(lines, sectionName) {
				if len(lines) > 0 && lines[len(lines)-1] != "" {
					lines = append(lines, "")
				}
				lines = append(lines, fmt.Sprintf("[%s]", sectionName))
			}
			lines = append(lines, fmt.Sprintf("%s=%s", sectionKey, stringifyScalar(entry.ReplaceWith)))
		} else {
			lines = append(lines, fmt.Sprintf("%s=%s", key, stringifyScalar(entry.ReplaceWith)))
		}
	}

	result := strings.Join(lines, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

func hasSection(lines []string, section string) bool {
	needle := "[" + strings.TrimSpace(section) + "]"
	for _, line := range lines {
		if strings.TrimSpace(line) == needle {
			return true
		}
	}
	return false
}

func applyTextParser(content string, entries []ConfigReplaceEntry) string {
	out := content
	for _, entry := range entries {
		search := stringifyScalar(entry.Match)
		replace := stringifyScalar(entry.ReplaceWith)
		if entry.HasIfValue && !valuesEquivalent(search, entry.IfValue) {
			continue
		}
		if search == "" {
			continue
		}
		out = strings.ReplaceAll(out, search, replace)
	}
	return out
}

func applyFileParser(content string, entries []ConfigReplaceEntry) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	out := strings.Join(lines, "\n")

	for _, entry := range entries {
		search := strings.TrimSpace(stringifyScalar(entry.Match))
		replace := stringifyScalar(entry.ReplaceWith)
		if search == "" {
			continue
		}
		if entry.HasIfValue && !valuesEquivalent(search, entry.IfValue) {
			continue
		}

		updatedLines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
		replacedAny := false
		for i, line := range updatedLines {
			if fileLineMatches(line, search) {
				updatedLines[i] = replace
				replacedAny = true
			}
		}
		if replacedAny {
			out = strings.Join(updatedLines, "\n")
			continue
		}

		// For simple key matches (e.g. "port"), never do global substring replacement.
		// Append or upsert safely instead of mutating random substrings in the file.
		if isSimpleConfigToken(search) {
			out = appendConfigLine(out, search, replace)
			continue
		}

		// Fallback for templates relying on pure token replacement with non-key patterns.
		out = strings.ReplaceAll(out, search, replace)
	}

	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

func fileLineMatches(line string, search string) bool {
	trimmedLine := strings.TrimSpace(line)
	trimmedSearch := strings.TrimSpace(search)
	if trimmedLine == "" || trimmedSearch == "" {
		return false
	}
	if trimmedLine == trimmedSearch {
		return true
	}
	if strings.Contains(trimmedSearch, "=") || strings.Contains(trimmedSearch, ":") || strings.Contains(trimmedSearch, " ") {
		return strings.Contains(trimmedLine, trimmedSearch)
	}
	if !isSimpleConfigToken(trimmedSearch) {
		return strings.Contains(trimmedLine, trimmedSearch)
	}

	if !strings.HasPrefix(trimmedLine, trimmedSearch) {
		return false
	}
	if len(trimmedLine) == len(trimmedSearch) {
		return true
	}
	next := trimmedLine[len(trimmedSearch)]
	return next == ' ' || next == '\t' || next == '=' || next == ':'
}

func appendConfigLine(content string, key string, replace string) string {
	out := content
	trimmedReplace := strings.TrimSpace(replace)
	if trimmedReplace == "" {
		return out
	}

	if !strings.Contains(trimmedReplace, "=") && !strings.Contains(trimmedReplace, ":") {
		trimmedReplace = key + "=" + trimmedReplace
	}

	if strings.TrimSpace(out) == "" {
		return trimmedReplace
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + trimmedReplace
}

func isSimpleConfigToken(value string) bool {
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func stringifyScalar(value interface{}) string {
	if value == nil {
		return ""
	}
	switch t := value.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		if t == float32(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(float64(t), 'f', -1, 64)
	case int, int32, int64:
		return asString(t)
	default:
		raw, _ := json.Marshal(t)
		if len(raw) == 0 || string(raw) == "null" {
			return ""
		}
		return string(raw)
	}
}

func coerceTypedValue(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	if str, ok := value.(string); ok {
		trimmed := strings.TrimSpace(str)
		if trimmed == "" {
			return ""
		}
		if strings.EqualFold(trimmed, "true") {
			return true
		}
		if strings.EqualFold(trimmed, "false") {
			return false
		}
		if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f
		}
		if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) || (strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
			var payload interface{}
			if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
				return payload
			}
		}
		return trimmed
	}
	return value
}

func valuesEquivalent(a interface{}, b interface{}) bool {
	av := coerceTypedValue(a)
	bv := coerceTypedValue(b)

	rawA, _ := json.Marshal(av)
	rawB, _ := json.Marshal(bv)
	if string(rawA) == string(rawB) {
		return true
	}
	return stringifyScalar(av) == stringifyScalar(bv)
}

func applyStructuredReplacements(root *interface{}, entries []ConfigReplaceEntry) {
	for _, entry := range entries {
		segments := parseConfigPathSegments(entry.Match)
		if len(segments) == 0 {
			continue
		}

		replaceWith := coerceTypedValue(entry.ReplaceWith)
		expected := coerceTypedValue(entry.IfValue)

		paths := collectTargetPaths(*root, segments, 0, nil)
		if len(paths) == 0 && !containsWildcardSegment(segments) {
			paths = append(paths, segments)
		}

		for _, concrete := range paths {
			current, exists := getNestedValue(*root, concrete)
			if entry.HasIfValue {
				if !exists || !valuesEquivalent(current, expected) {
					continue
				}
			}
			setNestedValue(root, concrete, replaceWith)
		}
	}
}

func parseConfigPathSegments(expression string) []string {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return nil
	}

	parts := strings.Split(expression, ".")
	segments := make([]string, 0, len(parts))
	re := regexp.MustCompile(`([^[\]]+)|\[(\d+|\*)\]`)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		matches := re.FindAllStringSubmatch(part, -1)
		if len(matches) == 0 {
			segments = append(segments, part)
			continue
		}
		for _, m := range matches {
			if len(m) < 3 {
				continue
			}
			if strings.TrimSpace(m[1]) != "" {
				segments = append(segments, strings.TrimSpace(m[1]))
			} else if strings.TrimSpace(m[2]) != "" {
				segments = append(segments, strings.TrimSpace(m[2]))
			}
		}
	}
	return segments
}

func containsWildcardSegment(segments []string) bool {
	for _, segment := range segments {
		if segment == "*" {
			return true
		}
	}
	return false
}

func collectTargetPaths(node interface{}, segments []string, idx int, prefix []string) [][]string {
	if idx >= len(segments) {
		clone := append([]string{}, prefix...)
		return [][]string{clone}
	}

	segment := segments[idx]
	if segment == "*" {
		paths := [][]string{}
		switch t := node.(type) {
		case map[string]interface{}:
			for key, value := range t {
				paths = append(paths, collectTargetPaths(value, segments, idx+1, append(prefix, key))...)
			}
		case []interface{}:
			for i, value := range t {
				paths = append(paths, collectTargetPaths(value, segments, idx+1, append(prefix, strconv.Itoa(i)))...)
			}
		}
		return paths
	}

	if arrIndex, err := strconv.Atoi(segment); err == nil {
		array, ok := node.([]interface{})
		if !ok || arrIndex < 0 || arrIndex >= len(array) {
			return nil
		}
		return collectTargetPaths(array[arrIndex], segments, idx+1, append(prefix, strconv.Itoa(arrIndex)))
	}

	mapped, ok := node.(map[string]interface{})
	if !ok {
		return nil
	}
	child, exists := mapped[segment]
	if !exists {
		return nil
	}
	return collectTargetPaths(child, segments, idx+1, append(prefix, segment))
}

func getNestedValue(root interface{}, segments []string) (interface{}, bool) {
	current := root
	for _, segment := range segments {
		if idx, err := strconv.Atoi(segment); err == nil {
			arr, ok := current.([]interface{})
			if !ok || idx < 0 || idx >= len(arr) {
				return nil, false
			}
			current = arr[idx]
			continue
		}

		mapped, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		next, exists := mapped[segment]
		if !exists {
			return nil, false
		}
		current = next
	}
	return current, true
}

func setNestedValue(root *interface{}, segments []string, value interface{}) {
	if len(segments) == 0 {
		*root = value
		return
	}
	setNestedRecursive(root, segments, 0, value)
}

func setNestedRecursive(current *interface{}, segments []string, idx int, value interface{}) {
	segment := segments[idx]
	isLast := idx == len(segments)-1

	if arrIndex, err := strconv.Atoi(segment); err == nil {
		arr, ok := (*current).([]interface{})
		if !ok {
			arr = []interface{}{}
		}
		for len(arr) <= arrIndex {
			arr = append(arr, nil)
		}
		if isLast {
			arr[arrIndex] = value
			*current = arr
			return
		}
		next := arr[arrIndex]
		setNestedRecursive(&next, segments, idx+1, value)
		arr[arrIndex] = next
		*current = arr
		return
	}

	mapped, ok := (*current).(map[string]interface{})
	if !ok {
		mapped = map[string]interface{}{}
	}
	if isLast {
		mapped[segment] = value
		*current = mapped
		return
	}

	next := mapped[segment]
	setNestedRecursive(&next, segments, idx+1, value)
	mapped[segment] = next
	*current = mapped
}
