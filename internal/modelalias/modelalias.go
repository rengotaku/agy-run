// Package modelalias loads, resolves, and validates model alias chains from models.json.
package modelalias

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Chains maps alias names to prioritized model ID lists.
type Chains map[string][]string

// UnknownAliasError is returned by Resolve when the requested alias is not found.
type UnknownAliasError struct {
	Name  string
	Known []string
}

func (e *UnknownAliasError) Error() string {
	return fmt.Sprintf("unknown model alias %q (known: %s)", e.Name, strings.Join(e.Known, ", "))
}

// DefaultPath returns the path to models.json by priority:
// 1. AGY_RUN_MODELS_FILE
// 2. $XDG_CONFIG_HOME/agy-run/models.json
// 3. <home>/.config/agy-run/models.json
func DefaultPath(getenv func(string) string, home string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := getenv("AGY_RUN_MODELS_FILE"); v != "" {
		return v
	}
	if v := getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "agy-run", "models.json")
	}
	return filepath.Join(home, ".config", "agy-run", "models.json")
}

// Load reads and parses models.json from path.
// It verifies that "chains" is present and an object, and that each chain is
// a non-empty array of non-empty strings. Unknown top-level keys are ignored.
// On any error, path is included in the error message.
func Load(path string) (Chains, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read models file %s: %w", path, err)
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("failed to parse models file %s: %w", path, err)
	}

	chainsRaw, ok := root["chains"]
	if !ok {
		return nil, fmt.Errorf("models file %s missing \"chains\" object", path)
	}

	var rawChains map[string]json.RawMessage
	if err := json.Unmarshal(chainsRaw, &rawChains); err != nil {
		return nil, fmt.Errorf("models file %s: chains must be an object: %w", path, err)
	}
	if rawChains == nil {
		return nil, fmt.Errorf("models file %s: chains must be an object (got null)", path)
	}

	result := make(Chains, len(rawChains))
	for name, chainRaw := range rawChains {
		var items []any
		if err := json.Unmarshal(chainRaw, &items); err != nil {
			return nil, fmt.Errorf("models file %s: chain %q must be an array: %w", path, name, err)
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("models file %s: chain %q is empty", path, name)
		}
		models := make([]string, 0, len(items))
		for i, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("models file %s: chain %q item %d is not a string", path, name, i)
			}
			if s == "" {
				return nil, fmt.Errorf("models file %s: chain %q item %d is empty string", path, name, i)
			}
			models = append(models, s)
		}
		result[name] = models
	}

	return result, nil
}

// Resolve returns a copy of the model chain for the given alias name.
// If the alias is not found, an *UnknownAliasError with sorted known aliases is returned.
func (c Chains) Resolve(name string) ([]string, error) {
	chain, ok := c[name]
	if !ok {
		known := make([]string, 0, len(c))
		for k := range c {
			known = append(known, k)
		}
		sort.Strings(known)
		return nil, &UnknownAliasError{
			Name:  name,
			Known: known,
		}
	}
	copied := make([]string, len(chain))
	copy(copied, chain)
	return copied, nil
}

// FilterAvailable splits chain into models present in available (kept)
// and those absent (dropped), preserving the original order in both.
func FilterAvailable(chain, available []string) (kept, dropped []string) {
	availSet := make(map[string]struct{}, len(available))
	for _, a := range available {
		availSet[a] = struct{}{}
	}
	for _, m := range chain {
		if _, ok := availSet[m]; ok {
			kept = append(kept, m)
		} else {
			dropped = append(dropped, m)
		}
	}
	return kept, dropped
}

var validAliasRegex = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ParseRef checks whether model is an alias reference (@name).
// If model (trimmed) starts with '@', it validates the alias name against [a-z0-9_-]+.
// Mixing alias reference with model lists or invalid alias syntax returns an error.
// If model is a standard model specification without '@', it returns ("", false, nil).
func ParseRef(model string) (string, bool, error) {
	s := strings.TrimSpace(model)
	if strings.HasPrefix(s, "@") {
		if strings.Contains(s, ",") {
			return "", false, fmt.Errorf("invalid model alias %q: mixing alias with model list is not allowed", model)
		}
		name := s[1:]
		if name == "" {
			return "", false, fmt.Errorf("invalid model alias %q: alias name cannot be empty", model)
		}
		if !validAliasRegex.MatchString(name) {
			return "", false, fmt.Errorf("invalid model alias %q: name must match [a-z0-9_-]+", model)
		}
		return name, true, nil
	}
	if strings.Contains(s, "@") {
		return "", false, fmt.Errorf("invalid model alias %q: mixing alias with model list is not allowed", model)
	}
	return "", false, nil
}
