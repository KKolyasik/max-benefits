package contract

import (
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/hamba/avro/v2"
)

//go:embed avro/*.avsc
var avscFiles embed.FS

// schema is the Avro schema of a message type.
type schema struct {
	// subject is the full name of the record, the subject in the registry.
	subject string
	avro    avro.Schema
	// json is the schema for the registry: self-contained, with the docs and
	// the defaults as written.
	json string
}

// files maps the schema files to the message types.
var files = []struct {
	name string
	typ  reflect.Type
}{
	{"card.avsc", reflect.TypeFor[Card]()},
	{"survey.avsc", reflect.TypeFor[Survey]()},
	{"draft.avsc", reflect.TypeFor[Draft]()},
	{"decision.avsc", reflect.TypeFor[Decision]()},
	{"command.avsc", reflect.TypeFor[RunCommand]()},
	{"run.avsc", reflect.TypeFor[RunReport]()},
}

// schemas holds the schema of every message type.
var schemas = mustLoadSchemas()

func mustLoadSchemas() map[reflect.Type]*schema {
	s, err := loadSchemas()
	if err != nil {
		panic(err)
	}
	return s
}

func loadSchemas() (map[reflect.Type]*schema, error) {
	roots := make([]map[string]any, len(files))
	defs := map[string]map[string]any{}
	for i, f := range files {
		data, err := avscFiles.ReadFile("avro/" + f.name)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &roots[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		name, _ := roots[i]["name"].(string)
		ns, _ := roots[i]["namespace"].(string)
		defs[fullName(name, ns)] = roots[i]
	}

	out := map[reflect.Type]*schema{}
	for i, f := range files {
		bundled, err := json.Marshal(bundle(roots[i], defs))
		if err != nil {
			return nil, err
		}
		// A cache of its own: the schema must stand alone in the registry.
		parsed, err := avro.ParseBytesWithCache(bundled, "", &avro.SchemaCache{})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		named, ok := parsed.(avro.NamedSchema)
		if !ok {
			return nil, fmt.Errorf("%s: a message must be a record", f.name)
		}
		out[f.typ] = &schema{subject: named.FullName(), avro: parsed, json: string(bundled)}
	}
	return out, nil
}

// bundle makes a schema self-contained: a record of another file is written
// in full where it is first used, as Avro requires, instead of by name. So a
// record is defined once in the files and still registers on its own.
func bundle(root map[string]any, defs map[string]map[string]any) map[string]any {
	defined := map[string]bool{}
	var walkType func(t any, ns string) any
	var walkComplex func(m map[string]any, ns string) map[string]any
	walkType = func(t any, ns string) any {
		switch t := t.(type) {
		case string:
			if def, ok := defs[fullName(t, ns)]; ok && !defined[fullName(t, ns)] {
				return walkComplex(def, ns)
			}
			return t
		case []any: // a union
			out := make([]any, len(t))
			for i, u := range t {
				out[i] = walkType(u, ns)
			}
			return out
		case map[string]any:
			return walkComplex(t, ns)
		}
		return t
	}
	walkComplex = func(m map[string]any, ns string) map[string]any {
		out := maps.Clone(m)
		switch m["type"] {
		case "record", "error", "enum", "fixed":
			name, _ := m["name"].(string)
			if explicit, ok := m["namespace"].(string); ok {
				ns = explicit
			}
			full := fullName(name, ns)
			defined[full] = true
			if i := strings.LastIndex(full, "."); i >= 0 {
				ns = full[:i]
			}
			if fields, ok := m["fields"].([]any); ok {
				walked := make([]any, len(fields))
				for i, f := range fields {
					field := maps.Clone(f.(map[string]any))
					field["type"] = walkType(field["type"], ns)
					walked[i] = field
				}
				out["fields"] = walked
			}
		case "array":
			out["items"] = walkType(m["items"], ns)
		case "map":
			out["values"] = walkType(m["values"], ns)
		}
		return out
	}
	return walkComplex(root, "")
}

// fullName is the full name of a named type in a namespace.
func fullName(name, ns string) string {
	if strings.Contains(name, ".") || ns == "" {
		return name
	}
	return ns + "." + name
}
