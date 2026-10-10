package operations

import (
	"bytes"
	"encoding/json"
)

var registry map[string]Operation
var declarationOrder []string
var itemSchemas map[string]object
var legacySeeds map[string][]any

func init() {
	var declarations []Operation
	decode := func(text string, target any) {
		d := json.NewDecoder(bytes.NewBufferString(text))
		d.UseNumber()
		if err := d.Decode(target); err != nil {
			panic(err)
		}
	}
	decode(declarationsJSON, &declarations)
	decode(itemSchemasJSON, &itemSchemas)
	decode(legacySeedsJSON, &legacySeeds)
	registry = map[string]Operation{}
	for _, op := range declarations {
		op.handler = handlerFor(op.Name)
		registry[op.Name] = op
		declarationOrder = append(declarationOrder, op.Name)
	}
}

// Registry returns detached metadata in authored declaration order. Callers
// cannot mutate the service's validator or install another handler.
func Registry() []Operation {
	out := []Operation{}
	for _, name := range declarationOrder {
		op := registry[name]
		op.Input = clone(op.Input).(object)
		op.handler = nil
		out = append(out, op)
	}
	return out
}
func operationNames() []string { return append([]string{}, declarationOrder...) }
func handlerFor(name string) func(*execution, object) (any, error) {
	switch name {
	case "contract":
		return func(_ *execution, _ object) (any, error) { return Registry(), nil }
	case "schema":
		return schemaView
	case "board":
		return board
	case "databases":
		return databases
	case "list":
		return list
	case "get":
		return get
	case "search":
		return search
	case "create":
		return create
	case "update":
		return update
	case "comment":
		return comment
	case "link", "unlink":
		return func(x *execution, in object) (any, error) { return itemLink(x, in, name == "link") }
	case "link_add", "link_remove":
		return func(x *execution, in object) (any, error) { return externalLink(x, in, name == "link_add") }
	case "reorder":
		return reorder
	case "decide", "defer", "reopen":
		return func(x *execution, in object) (any, error) { return decision(x, in, name) }
	case "inbox_add":
		return inboxAdd
	case "inbox_promote":
		return inboxPromote
	case "inbox_dismiss":
		return inboxDismiss
	case "migrate":
		return migrate
	default:
		return func(x *execution, in object) (any, error) { return x.upstream(name, in) }
	}
}
