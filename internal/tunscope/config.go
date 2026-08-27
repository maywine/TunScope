package tunscope

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxApplicationTargets = 128

func validateApplicationPaths(values []string) ([]string, error) {
	_, resolved, err := validateApplicationTargets(values)
	return resolved, err
}

// validateApplicationTargets keeps both the stable configured path and its
// current resolved target. Versioned updaters commonly expose a `Current`
// symlink; the configured path must survive so the matcher can discover the
// next version after that symlink moves.
func validateApplicationTargets(values []string) ([]string, []string, error) {
	if len(values) > maxApplicationTargets {
		return nil, nil, fmt.Errorf("at most %d applications may be selected", maxApplicationTargets)
	}

	seen := make(map[string]struct{}, len(values))
	configured := make([]string, 0, len(values))
	resolvedPaths := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" || !filepath.IsAbs(value) {
			return nil, nil, fmt.Errorf("application path must be absolute: %q", raw)
		}
		value = filepath.Clean(value)
		resolved, err := filepath.EvalSymlinks(value)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve application path %q: %w", value, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect application path %q: %w", resolved, err)
		}
		if info.IsDir() && !strings.EqualFold(filepath.Ext(resolved), ".app") {
			return nil, nil, fmt.Errorf("application directory must end in .app: %q", resolved)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("application path is not a regular file: %q", resolved)
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		configured = append(configured, value)
		resolvedPaths = append(resolvedPaths, resolved)
	}
	type targetPair struct{ configured, resolved string }
	pairs := make([]targetPair, len(configured))
	for i := range configured {
		pairs[i] = targetPair{configured: configured[i], resolved: resolvedPaths[i]}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].resolved < pairs[j].resolved })
	for i := range pairs {
		configured[i] = pairs[i].configured
		resolvedPaths[i] = pairs[i].resolved
	}
	return configured, resolvedPaths, nil
}

func validatePackageFamilyNames(values []string) ([]string, error) {
	if len(values) > maxApplicationTargets {
		return nil, fmt.Errorf("at most %d package families may be selected", maxApplicationTargets)
	}

	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		separator := strings.LastIndexByte(value, '_')
		if separator < 3 || separator > 50 || len(value)-separator-1 != 13 {
			return nil, fmt.Errorf("invalid Windows package family name %q", raw)
		}
		for _, character := range value[:separator] {
			if !isASCIILetterOrDigit(character) && character != '.' && character != '-' {
				return nil, fmt.Errorf("invalid Windows package family name %q", raw)
			}
		}
		for _, character := range value[separator+1:] {
			if !isASCIILetterOrDigit(character) {
				return nil, fmt.Errorf("invalid Windows package family name %q", raw)
			}
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i]) < strings.ToLower(result[j])
	})
	return result, nil
}

func validateApplicationTargetCount(applicationPaths, packageFamilies []string) error {
	if len(applicationPaths)+len(packageFamilies) > maxApplicationTargets {
		return fmt.Errorf("at most %d application paths and package families may be selected", maxApplicationTargets)
	}
	return nil
}

func isASCIILetterOrDigit(character rune) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}
