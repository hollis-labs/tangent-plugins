package operations

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func validate(schema object, value any, root object, path string) []problem {
	out := []problem{}
	add := func(message string) {
		p := path
		if p == "" {
			p = "(root)"
		}
		out = append(out, problem{p, message})
	}
	if ref := str(schema["$ref"]); ref != "" {
		var target any = root
		for _, key := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, _ := target.(object)
			target = m[key]
		}
		if m, ok := target.(object); ok {
			return validate(m, value, root, path)
		}
		return out
	}
	for _, keyword := range []string{"oneOf", "anyOf"} {
		if alternatives, ok := schema[keyword].([]any); ok {
			matches := 0
			for _, alternative := range alternatives {
				if len(validate(alternative.(object), value, root, path)) == 0 {
					matches++
				}
			}
			if matches == 0 || keyword == "oneOf" && matches != 1 {
				add("must match " + keyword + " alternatives")
			}
		}
	}
	if denied, ok := schema["not"].(object); ok && len(validate(denied, value, root, path)) == 0 {
		add("field is not allowed")
	}
	if v, exists := schema["const"]; exists && !reflect.DeepEqual(value, v) {
		add("must be " + jsonText(v))
	}
	if vals, ok := schema["enum"].([]any); ok {
		found := false
		labels := []string{}
		for _, v := range vals {
			found = found || reflect.DeepEqual(value, v)
			labels = append(labels, jsString(v))
		}
		if !found {
			add("must be one of " + strings.Join(labels, ", "))
		}
	}
	if typ := str(schema["type"]); typ != "" && !isType(value, typ) {
		add("must be " + typ)
		return out
	}
	if n, ok := number(value); ok {
		if lowerBound, has := number(schema["minimum"]); has && n < lowerBound {
			add("must be >= " + jsString(schema["minimum"]))
		}
		if upperBound, has := number(schema["maximum"]); has && n > upperBound {
			add("must be <= " + jsString(schema["maximum"]))
		}
	}
	if list, ok := value.([]any); ok {
		if lowerBound, ok := number(schema["minItems"]); ok && float64(len(list)) < lowerBound {
			add("too few entries")
		}
		if upperBound, ok := number(schema["maxItems"]); ok && float64(len(list)) > upperBound {
			add("too many entries")
		}
		if rule, has := schema["items"].(object); has {
			for i, v := range list {
				out = append(out, validate(rule, v, root, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}
	if text, ok := value.(string); ok {
		if lowerBound, ok := number(schema["minLength"]); ok && float64(len([]rune(text))) < lowerBound {
			add("string too short")
		}
		if upperBound, ok := number(schema["maxLength"]); ok && float64(len([]rune(text))) > upperBound {
			add("string too long")
		}
	}
	if m, ok := value.(object); ok {
		required, _ := schema["required"].([]any)
		sub := func(key string) string {
			if path == "" {
				return key
			}
			return path + "." + key
		}
		for _, r := range required {
			k := str(r)
			v, has := m[k]
			if !has || v == nil || v == "" {
				out = append(out, problem{sub(k), "is required"})
			}
		}
		props, _ := schema["properties"].(object)
		keys := []string{}
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if rule, has := props[k].(object); has {
				out = append(out, validate(rule, m[k], root, sub(k))...)
			} else if schema["additionalProperties"] == false {
				out = append(out, problem{sub(k), "unknown field"})
			}
		}
	}
	return out
}
func isType(v any, typ string) bool {
	switch typ {
	case "null":
		return v == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "object":
		_, ok := v.(object)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		_, ok := integer(v)
		return ok
	case "number":
		_, ok := number(v)
		return ok
	}
	return false
}
func (x *execution) check(db string, item object) error {
	schema := itemSchemas[db]
	if errors := validate(schema["item"].(object), item, schema, ""); len(errors) > 0 {
		return failure("invalid", db+" item "+str(item["id"])+" failed validation", object{"errors": errors})
	}
	return nil
}
func schemaView(_ *execution, in object) (any, error) {
	db := str(in["db"])
	if err := knownDB(db); err != nil {
		return nil, err
	}
	schema := itemSchemas[db]
	item := schema["item"].(object)
	props := item["properties"].(object)
	fields, enums := object{}, object{}
	for name, v := range props {
		f := v.(object)
		view := object{}
		for _, key := range []string{"type", "const", "enum", "minimum", "maximum", "format", "description"} {
			if v, has := f[key]; has {
				view[key] = v
			}
		}
		ref := func(f object) string { return strings.TrimPrefix(str(f["$ref"]), "#/$defs/") }
		if r := ref(f); r != "" {
			view["ref"] = r
		}
		if child, has := f["items"].(object); has {
			v := ref(child)
			if v == "" {
				v = str(child["type"])
			}
			if v == "" {
				v = "any"
			}
			view["items"] = v
			if e, has := child["enum"]; has {
				enums[name] = e
			}
		}
		if e, has := f["enum"]; has {
			enums[name] = e
		}
		fields[name] = view
	}
	defs := schema["$defs"].(object)
	link := defs["link"].(object)
	kinds := link["properties"].(object)["kind"].(object)["enum"]
	return object{"db": db, "id": schema["$id"], "description": schema["description"], "required": item["required"], "fields": fields, "enums": enums, "link_kinds": kinds}, nil
}
