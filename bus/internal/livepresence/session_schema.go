package livepresence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"
)

type SessionSchema struct{ definitions map[string]*jsonschema.Schema }

var sessionSchemaKeywords = map[string]bool{
	"$ref": true, "type": true, "additionalProperties": true, "required": true,
	"properties": true, "allOf": true, "if": true, "then": true, "else": true,
	"not": true, "items": true, "uniqueItems": true, "enum": true, "const": true,
	"minLength": true, "maxLength": true, "minimum": true,
}

func CompileSessionSchema(raw []byte) (*SessionSchema, error) {
	var root map[string]any
	if err := decodeSessionJSON(raw, &root); err != nil {
		return nil, err
	}
	for key := range root {
		if key != "$schema" && key != "$id" && key != "$defs" {
			return nil, fmt.Errorf("unsupported schema root key %q", key)
		}
	}
	defs, ok := root["$defs"].(map[string]any)
	if !ok || len(defs) == 0 {
		return nil, errors.New("session schema has no definitions")
	}
	compiler := jsonschema.NewCompiler()
	const resource = "https://agent-sessions.invalid/session.schema.json"
	if err := compiler.AddResource(resource, bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	if _, err := compiler.Compile(resource); err != nil {
		return nil, err
	}
	compiled := make(map[string]*jsonschema.Schema, len(defs))
	for name, node := range defs {
		if err := checkSessionSchemaNode(node); err != nil {
			return nil, fmt.Errorf("definition %s: %w", name, err)
		}
		schema, err := compiler.Compile(resource + "#/$defs/" + name)
		if err != nil {
			return nil, fmt.Errorf("compile definition %s: %w", name, err)
		}
		compiled[name] = schema
	}
	return &SessionSchema{definitions: compiled}, nil
}
func (s *SessionSchema) Definitions() []string {
	names := make([]string, 0, len(s.definitions))
	for name := range s.definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func (s *SessionSchema) ValidateJSON(name string, raw []byte) error {
	schema, ok := s.definitions[name]
	if !ok {
		return fmt.Errorf("unknown session schema definition %q", name)
	}
	var value any
	if err := decodeSessionJSON(raw, &value); err != nil {
		return err
	}
	return schema.Validate(value)
}

func decodeSessionJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("session JSON has trailing values")
	}
	return nil
}

func checkSessionSchemaNode(value any) error {
	node, ok := value.(map[string]any)
	if !ok {
		return errors.New("schema node is not an object")
	}
	for key, child := range node {
		if !sessionSchemaKeywords[key] {
			return fmt.Errorf("unsupported schema keyword %q", key)
		}
		switch key {
		case "properties":
			for _, property := range child.(map[string]any) {
				if err := checkSessionSchemaNode(property); err != nil {
					return err
				}
			}
		case "items", "if", "then", "else", "not":
			if err := checkSessionSchemaNode(child); err != nil {
				return err
			}
		case "allOf":
			for _, item := range child.([]any) {
				if err := checkSessionSchemaNode(item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
