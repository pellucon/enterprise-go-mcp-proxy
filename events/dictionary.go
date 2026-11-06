// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package events

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// MetadataFieldSpec describes one metadata key an EventTypeSpec expects:
// its value type, whether it's mandatory, and (for "string" fields) an
// optional closed set of allowed values — e.g. a "severity" key restricted
// to "warning"/"error".
type MetadataFieldSpec struct {
	// Type is one of "string", "number", "date", "boolean", "json" ("json"
	// accepts any value shape, for nested/opaque JSON like a request or
	// result payload).
	Type string `json:"type"`

	// Required, if true, means Validate rejects an Event missing this key.
	Required bool `json:"required"`

	// AllowedValues, if non-empty, restricts a "string"-typed value to this
	// closed set. Ignored for other Type values.
	AllowedValues []string `json:"allowedValues,omitempty"`
}

// EventTypeSpec is one event type's entry in the Dictionary: the closed
// set of subtypes that type may use (if any), and the metadata keys
// Validate checks for that type.
type EventTypeSpec struct {
	// Subtypes, if non-empty, is the closed set of subtypes this event
	// type may be published with — e.g. "alert" events are restricted to
	// "tool-call-rejected"/"result-rejected". Empty means this event type
	// doesn't use subtypes (an event of this type must then have an empty
	// Subtype - see Validate).
	Subtypes []string `json:"subtypes,omitempty"`

	Metadata map[string]MetadataFieldSpec `json:"metadata"`
}

// Dictionary is the JSON-authored description of every event type an
// events.Client is allowed to publish/expects to receive (per the original
// event-system spec: "a json based dictionary to describe all the possible
// events").
type Dictionary struct {
	EventTypes map[string]EventTypeSpec `json:"eventTypes"`
}

// ParseDictionary parses a Dictionary from its JSON file contents and
// validates its own shape (validateShape) — this is a config-shape check
// only, distinct from Validate(e *Event) below, which checks a specific
// event against an already-parsed, presumed-well-formed Dictionary.
func ParseDictionary(data []byte) (*Dictionary, error) {
	var d Dictionary
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("invalid event dictionary json: %w", err)
	}
	if err := d.validateShape(); err != nil {
		return nil, err
	}
	return &d, nil
}

// validateShape fails closed on a dictionary whose declared shape is
// itself invalid — today, only an unsupported MetadataFieldSpec.Type
// (anything other than validateFieldType's known set). Checking this at
// load time, rather than only when some event happens to use that
// metadata key, matches every other section's "fail-closed validation on
// every load" convention.
// subjectToken is what an event type or subtype name may consist of.
var subjectToken = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (d *Dictionary) validateShape() error {
	for typeName, spec := range d.EventTypes {
		// A type and subtype become tokens of the event's NATS subject
		// ("events.<type>.<subtype>"): ".", "*", ">" or whitespace in one
		// would change which subject it is, or which subscriptions match.
		if !subjectToken.MatchString(typeName) {
			return fmt.Errorf("event type %q: names may only use letters, digits, '-' and '_'", typeName)
		}
		for _, subtype := range spec.Subtypes {
			if !subjectToken.MatchString(subtype) {
				return fmt.Errorf("event type %q subtype %q: names may only use letters, digits, '-' and '_'", typeName, subtype)
			}
		}
		for key, fieldSpec := range spec.Metadata {
			switch fieldSpec.Type {
			case "string", "number", "date", "boolean", "json":
			default:
				return fmt.Errorf("event type %q metadata key %q: unsupported field type %q", typeName, key, fieldSpec.Type)
			}
		}
	}
	return nil
}

// Validate checks e against the Dictionary, failing closed: an unknown
// event type, a subtype outside that type's declared Subtypes (when it
// declares any), a missing required metadata key, a metadata value whose
// Go type doesn't match its declared Type, or a "string" value outside a
// declared AllowedValues set, are all errors.
func (d *Dictionary) Validate(e *Event) error {
	spec, ok := d.EventTypes[e.Type]
	if !ok {
		return fmt.Errorf("unknown event type %q", e.Type)
	}

	if len(spec.Subtypes) == 0 {
		if e.Subtype != "" {
			return fmt.Errorf("event type %q does not use subtypes, but got subtype %q", e.Type, e.Subtype)
		}
	} else if !contains(spec.Subtypes, e.Subtype) {
		return fmt.Errorf("event type %q: subtype %q is not one of the declared subtypes %v", e.Type, e.Subtype, spec.Subtypes)
	}

	for key, fieldSpec := range spec.Metadata {
		value, present := e.Metadata[key]
		if !present {
			if fieldSpec.Required {
				return fmt.Errorf("event type %q missing required metadata key %q", e.Type, key)
			}
			continue
		}
		if err := validateFieldType(fieldSpec.Type, value); err != nil {
			return fmt.Errorf("event type %q metadata key %q: %w", e.Type, key, err)
		}
		if len(fieldSpec.AllowedValues) > 0 && fieldSpec.Type == "string" {
			str, _ := value.(string)
			if !contains(fieldSpec.AllowedValues, str) {
				return fmt.Errorf("event type %q metadata key %q: value %q is not one of the allowed values %v", e.Type, key, str, fieldSpec.AllowedValues)
			}
		}
	}
	return nil
}

func validateFieldType(fieldType string, value any) error {
	switch fieldType {
	case "string", "date":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("expected a string, got %T", value)
		}
	case "number":
		switch value.(type) {
		case float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			// ok
		default:
			return fmt.Errorf("expected a number, got %T", value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected a boolean, got %T", value)
		}
	case "json":
		// Any shape accepted - this represents opaque nested JSON (e.g. a
		// request or result payload).
	default:
		return fmt.Errorf("dictionary declares unsupported field type %q", fieldType)
	}
	return nil
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
