package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"gobackupper/internal/backup"
	"gobackupper/internal/pathresolve"
)

type Result struct {
	Plan     backup.Plan
	Warnings []string
}

func Load(filename string, invocationDir string) (Result, error) {
	var result Result
	main, err := parseFile(filename)
	if err != nil {
		return result, fmt.Errorf("load config %q: %w", filename, err)
	}
	target, err := main.readSection("target")
	if err != nil {
		return result, fmt.Errorf("config %q: %w", filename, err)
	}
	path, ok := target.get("path")
	if !ok || path == "" {
		return result, fmt.Errorf("config %q: [target] requires path", filename)
	}
	snapshots, ok := target.get("snapshots")
	if !ok || snapshots == "" {
		return result, fmt.Errorf("config %q: [target] requires snapshots", filename)
	}
	result.Plan.TargetRoot, err = canonical(invocationDir + "/" + path)
	if err != nil {
		return result, fmt.Errorf("resolve target.path: %w", err)
	}
	result.Plan.SnapshotRoot, err = canonical(invocationDir + "/" + snapshots)
	if err != nil {
		return result, fmt.Errorf("resolve target.snapshots: %w", err)
	}
	sources, err := main.readSection("sources")
	if err != nil {
		return result, fmt.Errorf("config %q: %w", filename, err)
	}
	positions := make(map[string]int)
	for _, rootValue := range sources.keys {
		root, err := canonical(rootValue.value)
		if err != nil {
			return result, fmt.Errorf("resolve source root %q: %w", rootValue.value, err)
		}
		ini := filepath.Join(root, ".backup.ini")
		info, statErr := os.Stat(ini)
		if os.IsNotExist(statErr) || (statErr == nil && !info.Mode().IsRegular()) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("optional config %q is not a regular file", ini))
			continue
		}
		if statErr != nil {
			return result, fmt.Errorf("inspect source config %q: %w", ini, statErr)
		}
		cfg, err := parseFile(ini)
		if os.IsNotExist(err) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("optional config %q not found", ini))
			continue
		}
		if err != nil {
			return result, fmt.Errorf("load source config %q: %w", ini, err)
		}
		refs, err := cfg.readSection("targets")
		if err != nil {
			return result, fmt.Errorf("config %q: %w", ini, err)
		}
		for _, ref := range refs.keys {
			name := ref.value
			section, err := cfg.readSection(name)
			if err != nil {
				return result, fmt.Errorf("config %q: target %q: %w", ini, ref.key, err)
			}
			source, ok := section.get("path")
			if !ok || source == "" {
				return result, fmt.Errorf("config %q: [%s] requires a non-empty path", ini, name)
			}
			var sourcePath string
			if filepath.IsAbs(source) {
				sourcePath = source
			} else {
				sourcePath, err = canonical(root + "/" + source)
				if err != nil {
					return result, fmt.Errorf("resolve [%s].path: %w", name, err)
				}
			}
			targetpath, hasTarget := section.get("targetpath")
			dest := ""
			prefix := ""
			if hasTarget {
				for _, component := range strings.Split(targetpath, "/") {
					if component == ".." {
						return result, fmt.Errorf("config %q: [%s].targetpath must not contain parent traversal", ini, name)
					}
				}
				dest = filepath.Clean(result.Plan.TargetRoot + "/" + targetpath)
				prefix = strings.ReplaceAll(targetpath, "/", "-")
			} else {
				base := ""
				if !strings.HasSuffix(sourcePath, "/") {
					base = filepath.Base(sourcePath)
				}
				dest = filepath.Clean(result.Plan.TargetRoot + "/" + base)
				prefix = base
			}
			deleteValue, _ := section.get("delete")
			job := backup.Job{Name: name, SourcePath: sourcePath, Destination: dest, SnapshotPrefix: prefix, Delete: strings.EqualFold(deleteValue, "true"), Compression: "zstd", Origin: backup.Origin{File: ini, Section: name}}
			if value, exists := section.get("exclude"); exists {
				for _, item := range strings.Split(value, ",") {
					job.Excludes = append(job.Excludes, strings.TrimSpace(item))
				}
			}
			if value, exists := section.get("compression"); exists {
				job.Compression = value
			}
			if index, exists := positions[name]; exists {
				result.Plan.Jobs[index] = job
			} else {
				positions[name] = len(result.Plan.Jobs)
				result.Plan.Jobs = append(result.Plan.Jobs, job)
			}
		}
	}
	if len(result.Plan.Jobs) == 0 {
		return result, fmt.Errorf("no backup jobs found")
	}
	return result, nil
}

func canonical(path string) (string, error) { return pathresolve.Real(path) }

type keyValue struct{ key, value string }
type section struct {
	keys     []keyValue
	values   map[string]string
	resolved bool
}

func (s *section) get(key string) (string, bool) {
	v, ok := s.values[strings.ToLower(key)]
	return v, ok
}

type ini struct {
	sections map[string]*section
	defaults *section
}

func parseFile(path string) (*ini, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f)
}
func parse(input *os.File) (*ini, error) {
	cfg := &ini{sections: make(map[string]*section), defaults: &section{values: make(map[string]string)}}
	current, sectionName := cfg.defaults, "DEFAULT"
	reader := bufio.NewReader(input)
	lineNo, lastKey, lastIndent := 0, "", -1
	sectionSeen := false
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) == 0 && readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, readErr
		}
		lineNo++
		raw := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			if lastKey != "" {
				current.values[lastKey] += "\n"
				current.keys[len(current.keys)-1].value = current.values[lastKey]
			}
		} else if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			// Comments do not end the preceding option.
		} else {
			indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
			if indent > lastIndent && lastKey != "" {
				current.values[lastKey] += "\n" + trimmed
				current.keys[len(current.keys)-1].value = current.values[lastKey]
			} else {
				lastKey, lastIndent = "", -1
				if strings.HasPrefix(trimmed, "[") {
					close := strings.IndexByte(trimmed, ']')
					if close < 0 {
						return nil, fmt.Errorf("line %d: malformed section", lineNo)
					}
					name := trimmed[1:close]
					if name == "" {
						return nil, fmt.Errorf("line %d: empty section", lineNo)
					}
					if name == "DEFAULT" {
						current, sectionName = cfg.defaults, name
					} else {
						if _, exists := cfg.sections[name]; exists {
							return nil, fmt.Errorf("line %d: duplicate section [%s]", lineNo, name)
						}
						current = &section{values: make(map[string]string)}
						cfg.sections[name], sectionName = current, name
					}
					sectionSeen = true
				} else {
					if !sectionSeen {
						return nil, fmt.Errorf("line %d: option outside a section", lineNo)
					}
					idx := strings.IndexAny(raw, "=:")
					if idx < 0 {
						return nil, fmt.Errorf("line %d: expected key=value", lineNo)
					}
					key := strings.TrimSpace(raw[:idx])
					if key == "" {
						return nil, fmt.Errorf("line %d: empty key", lineNo)
					}
					k := strings.ToLower(key)
					if _, exists := current.values[k]; exists {
						return nil, fmt.Errorf("line %d: duplicate key %q in [%s]", lineNo, key, sectionName)
					}
					value := strings.TrimSpace(raw[idx+1:])
					current.values[k] = value
					current.keys = append(current.keys, keyValue{k, value})
					lastKey, lastIndent = k, indent
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	// The INI format joins multiline values and strips trailing whitespace once.
	trimValues := func(s *section) {
		for i := range s.keys {
			s.keys[i].value = strings.TrimRightFunc(s.keys[i].value, unicode.IsSpace)
			s.values[s.keys[i].key] = s.keys[i].value
		}
	}
	trimValues(cfg.defaults)
	for _, s := range cfg.sections {
		trimValues(s)
	}
	for _, s := range cfg.sections {
		ownKeys := s.keys
		ownValues := s.values
		merged := make(map[string]string, len(cfg.defaults.values)+len(ownValues))
		ordered := make([]keyValue, 0, len(cfg.defaults.keys)+len(ownKeys))
		for _, kv := range cfg.defaults.keys {
			value := kv.value
			if own, ok := ownValues[kv.key]; ok {
				value = own
			}
			merged[kv.key] = value
			ordered = append(ordered, keyValue{kv.key, value})
		}
		for _, kv := range ownKeys {
			if _, inherited := cfg.defaults.values[kv.key]; !inherited {
				merged[kv.key] = kv.value
				ordered = append(ordered, kv)
			}
		}
		s.values, s.keys = merged, ordered
	}
	return cfg, nil
}

// Interpolation occurs when a section is accessed, not while reading unrelated sections.
func (cfg *ini) readSection(name string) (*section, error) {
	s, ok := cfg.sections[name]
	if !ok {
		return nil, fmt.Errorf("missing section [%s]", name)
	}
	if s.resolved {
		return s, nil
	}
	rawSection := &section{values: s.values}
	expandedValues := make(map[string]string, len(s.values))
	for _, kv := range s.keys {
		expanded, err := interpolate(kv.value, rawSection, cfg.defaults, make(map[string]bool))
		if err != nil {
			return nil, fmt.Errorf("[%s].%s: %w", name, kv.key, err)
		}
		expandedValues[kv.key] = expanded
	}
	s.values = expandedValues
	for i := range s.keys {
		s.keys[i].value = expandedValues[s.keys[i].key]
	}
	s.resolved = true
	return s, nil
}
func interpolate(v string, section, defaults *section, visiting map[string]bool) (string, error) {
	if len(visiting) >= 10 {
		return "", fmt.Errorf("interpolation depth exceeds 10")
	}
	var out strings.Builder
	for i := 0; i < len(v); {
		if v[i] != '%' {
			out.WriteByte(v[i])
			i++
			continue
		}
		if i+1 < len(v) && v[i+1] == '%' {
			out.WriteByte('%')
			i += 2
			continue
		}
		if i+2 < len(v) && v[i+1] == '(' {
			end := strings.Index(v[i+2:], ")s")
			if end < 0 {
				return "", fmt.Errorf("invalid interpolation syntax")
			}
			key := strings.ToLower(v[i+2 : i+2+end])
			if visiting[key] {
				return "", fmt.Errorf("interpolation cycle at %q", key)
			}
			val, ok := section.values[key]
			if !ok {
				val, ok = defaults.values[key]
			}
			if !ok {
				return "", fmt.Errorf("interpolation key %q not found", key)
			}
			visiting[key] = true
			expanded, err := interpolate(val, section, defaults, visiting)
			delete(visiting, key)
			if err != nil {
				return "", err
			}
			out.WriteString(expanded)
			i += 2 + end + 2
			continue
		}
		return "", fmt.Errorf("invalid interpolation syntax")
	}
	return out.String(), nil
}
