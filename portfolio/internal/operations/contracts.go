package operations

import (
	"encoding/json"
	"strings"
)

// The legacy service requires nonempty required strings. Advertise that same
// constraint so a host does not admit an empty field that runtime rejects.
func requiredStrings(value any) {
	switch schema := value.(type) {
	case object:
		props, _ := schema["properties"].(object)
		if required, ok := schema["required"].([]any); ok {
			for _, key := range required {
				if rule, ok := props[str(key)].(object); ok && rule["type"] == "string" {
					rule["minLength"] = json.Number("1")
				}
			}
		}
		for _, child := range schema {
			requiredStrings(child)
		}
	case []any:
		for _, child := range schema {
			requiredStrings(child)
		}
	}
}

func validateInput(op Operation, in object) error {
	if op.baseInput != nil {
		if problems := validate(op.baseInput, in, op.baseInput, ""); len(problems) > 0 {
			return failure("bad_request", "bad input for "+op.Name, object{"errors": problems})
		}
	}
	code := "bad_request"
	if op.Name == "create" || op.Name == "update" || op.Name == "list" {
		if err := knownDB(str(in["db"])); err != nil {
			return err
		}
		if op.Name != "list" {
			code = "invalid"
		}
	}
	if op.Name == "update" {
		patch := in["patch"].(object)
		for _, key := range []string{"id", "created", "comments", "rev"} {
			if _, has := patch[key]; has {
				return failure("bad_request", "cannot patch: "+key, object{"fields": []string{key}})
			}
		}
	}
	if problems := validate(op.Input, in, op.Input, ""); len(problems) > 0 {
		return failure(code, "bad input for "+op.Name, object{"errors": problems})
	}
	return nil
}

// composeInput keeps transport discovery and service admission on the same
// database-specific contract. Unknown item fields remain lossless extensions.
func composeInput(op Operation) object {
	input := clone(op.Input).(object)
	props := input["properties"].(object)
	if scopeOperation(op.Name) {
		props["scope"] = scopeSchema()
	}
	if op.Name == "list" || op.Name == "search" {
		props["page"] = object{"type": "object", "additionalProperties": false, "properties": object{
			"limit":    object{"type": "integer", "minimum": 1, "maximum": 200},
			"offset":   object{"type": "integer", "minimum": 0, "maximum": 100000},
			"snapshot": object{"type": "string", "minLength": 64, "maxLength": 64},
		}}
	}
	field := ""
	switch op.Name {
	case "create":
		field = "item"
	case "update":
		field = "patch"
	case "list":
		field = "filters"
	}
	if field == "" {
		return input
	}
	branches := []any{}
	for _, db := range dbNames {
		item := clone(itemSchemas[db]["item"]).(object)
		itemProps := item["properties"].(object)
		required := []any{}
		switch field {
		case "item":
			for _, value := range item["required"].([]any) {
				key := str(value)
				_, defaulted := defaults[db][key]
				if key != "id" && key != "rev" && key != "created" && key != "updated" && key != "author" && key != "added_by" && key != "decided_by" && !defaulted {
					required = append(required, value)
				}
			}
			// These fields are discarded or verifier/service-owned by build.
			for _, key := range []string{"comments", "rev", "author", "added_by", "decided_by"} {
				itemProps[key] = object{}
			}
		case "patch":
			for key, rule := range itemProps {
				itemProps[key] = object{"anyOf": []any{rule, object{"type": "null"}}}
			}
			for _, key := range []string{"id", "created", "comments", "rev"} {
				itemProps[key] = object{"not": object{}}
			}
			for _, key := range []string{"author", "added_by", "decided_by"} {
				itemProps[key] = object{}
			}
		case "filters":
			for key, value := range itemProps {
				rule := value.(object)
				if rule["type"] == "array" {
					itemProps[key] = rule["items"]
				}
			}
		}
		item["required"] = required
		// References are local to each database in the enclosing operation.
		prefix := "#/$defs/" + db + "/$defs/"
		rebaseRefs(item, prefix)
		defs, _ := input["$defs"].(object)
		if defs == nil {
			defs = object{}
			input["$defs"] = defs
		}
		dbDefs := clone(itemSchemas[db]["$defs"]).(object)
		rebaseRefs(dbDefs, prefix)
		defs[db] = object{"$defs": dbDefs}
		branches = append(branches, object{"type": "object", "required": []any{"db"}, "properties": object{
			"db": object{"const": db}, field: item,
		}})
	}
	input["oneOf"] = branches
	return input
}

func rebaseRefs(value any, prefix string) {
	switch v := value.(type) {
	case object:
		if ref := str(v["$ref"]); strings.HasPrefix(ref, "#/$defs/") {
			v["$ref"] = prefix + strings.TrimPrefix(ref, "#/$defs/")
		}
		for _, child := range v {
			rebaseRefs(child, prefix)
		}
	case []any:
		for _, child := range v {
			rebaseRefs(child, prefix)
		}
	}
}
