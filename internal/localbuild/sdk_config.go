// Copyright 2016 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Go adaptation of Google Cloud CLI 591.0.0:
// api_lib/cloudbuild/cloudbuild_util.py: SnakeToCamelString, SnakeToCamel
// api_lib/cloudbuild/config.py: FinalizeCloudbuildConfig (substitution merge).
// Changes: YAML nodes/local maps replace Python dicts and API messages; only
// supported local fields are decoded. Service-side expansion is separate.
package localbuild

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

func expandConfigAliases(node *yaml.Node, visiting map[*yaml.Node]bool) (*yaml.Node, error) {
	if visiting[node] {
		return nil, fmt.Errorf("recursive YAML alias")
	}
	visiting[node] = true
	defer delete(visiting, node)
	if node.Kind == yaml.AliasNode {
		return expandConfigAliases(node.Alias, visiting)
	}
	out := *node
	out.Anchor, out.Alias, out.Content = "", nil, nil
	if node.Kind == yaml.MappingNode {
		// yaml.v3 resolves merge keys when decoding a mapping. Keeping values
		// as nodes preserves their scalar tags and quotation styles.
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag != "!!str" && key.Tag != "!!merge" {
				return nil, fmt.Errorf("config map keys must be strings")
			}
		}
		var fields map[string]yaml.Node
		if err := node.Decode(&fields); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := fields[key]
			expanded, err := expandConfigAliases(&value, visiting)
			if err != nil {
				return nil, err
			}
			out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, expanded)
		}
	} else {
		for _, value := range node.Content {
			expanded, err := expandConfigAliases(value, visiting)
			if err != nil {
				return nil, err
			}
			out.Content = append(out.Content, expanded)
		}
	}
	return &out, nil
}

func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	leading := 0
	for leading < len(parts) && parts[leading] == "" {
		leading++
	}
	if leading == len(parts) {
		return strings.Repeat("_", leading-1)
	}
	parts = parts[leading:]
	parts[0] = strings.Repeat("_", leading) + parts[0]
	for i := 1; i < len(parts); i++ {
		chars := []rune(strings.ToLower(parts[i]))
		if len(chars) > 0 {
			chars[0] = unicode.ToUpper(chars[0])
		}
		parts[i] = string(chars)
	}
	return strings.Join(parts, "")
}

func normalizeConfigKeys(node *yaml.Node) {
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			original := key.Value
			key.Value = snakeToCamel(original)
			// Match config.py's _SKIP_CAMEL_CASE: map keys are user data.
			switch original {
			case "substitutions", "secretEnv", "secret_env", "envMap", "env_map":
				continue
			}
			normalizeConfigKeys(value)
		}
		return
	}
	for _, child := range node.Content {
		normalizeConfigKeys(child)
	}
}

func mergeSubstitutions(config, overrides map[string]string) (map[string]string, error) {
	merged := map[string]string{}
	// As in FinalizeCloudbuildConfig, validate the config before flag overrides.
	for key, value := range config {
		if !userKey.MatchString(key) {
			return nil, fmt.Errorf("substitution key %s does not respect format %s", key, userKey)
		}
		merged[key] = value
	}
	for key, value := range overrides {
		merged[key] = value
	}
	return merged, nil
}

// Match the SDK's API-message decoding before yaml.v3 can implicitly coerce
// numbers/booleans to strings. API int64 fields also accept decimal strings.
func normalizeConfigTypes(node *yaml.Node, typ reflect.Type, field string, nullable bool) error {
	if node.Kind == yaml.DocumentNode {
		return normalizeConfigTypes(node.Content[0], typ, field, nullable)
	}
	if node.Tag == "!!null" {
		if nullable {
			return nil
		}
		return fmt.Errorf("%s: repeated values may not be null", field)
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: expected a mapping", field)
		}
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			for j := 0; j < typ.NumField(); j++ {
				member := typ.Field(j)
				if key.Value == member.Tag.Get("yaml") {
					name := strings.TrimPrefix(field+"."+key.Value, ".")
					if err := normalizeConfigTypes(value, member.Type, name, true); err != nil {
						return err
					}
					break
				}
			}
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			return fmt.Errorf("%s: expected a list", field)
		}
		for i, value := range node.Content {
			if err := normalizeConfigTypes(value, typ.Elem(), fmt.Sprintf("%s[%d]", field, i), false); err != nil {
				return err
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: expected a mapping", field)
		}
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("%s: expected a string map key", field)
			}
			if err := normalizeConfigTypes(value, typ.Elem(), field+"."+key.Value, true); err != nil {
				return err
			}
		}
	case reflect.Int, reflect.String:
		if typ.Kind() == reflect.Int || field == "options.diskSizeGb" {
			value, err := sdkInteger(node)
			if err != nil {
				return fmt.Errorf("%s: %w", field, err)
			}
			node.Value = strconv.FormatInt(value, 10)
			node.Tag = "!!int"
			node.Style = 0
			if typ.Kind() == reflect.String {
				node.Tag = "!!str"
			}
		} else if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return fmt.Errorf("%s: expected a string", field)
		}
	case reflect.Bool:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
			return fmt.Errorf("%s: expected a boolean", field)
		}
	}
	return nil
}

func sdkInteger(node *yaml.Node) (int64, error) {
	var value int64
	var err error
	if node.Kind == yaml.ScalarNode {
		switch node.Tag {
		case "!!int":
			err = node.Decode(&value)
		case "!!str":
			value, err = parseSDKInteger(node.Value)
		case "!!bool":
			// Python's API message integer validator also accepts bool values.
			var b bool
			err = node.Decode(&b)
			if b {
				value = 1
			}
		default:
			err = fmt.Errorf("not an integer")
		}
	} else {
		err = fmt.Errorf("not a scalar")
	}
	if err != nil {
		return 0, fmt.Errorf("expected an int64 or decimal integer string, got %q", node.Value)
	}
	return value, nil
}

var sdkDecimalInteger = regexp.MustCompile(`^[+-]?[0-9]+(?:_[0-9]+)*$`)

func parseSDKInteger(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if !sdkDecimalInteger.MatchString(s) {
		return 0, fmt.Errorf("invalid decimal integer")
	}
	return strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
}
