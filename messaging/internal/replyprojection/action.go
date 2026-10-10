package replyprojection

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

func decodeAction(raw []byte, target any) bool {
	if !utf8.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		field, tokenErr := decoder.Token()
		name, ok := field.(string)
		if tokenErr != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		if name == "interrupt" && !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
			return false
		}
	}
	if _, err = decoder.Token(); err != nil || !seen["item_id"] || !seen["expected_version"] || !seen["action_id"] {
		return false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return false
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil
}
