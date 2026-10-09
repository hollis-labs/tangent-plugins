package summarizer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

// strictDecode rejects trailing values, extra fields, duplicate decoded keys,
// excessive nesting, invalid UTF-8 and unpaired escaped Unicode surrogates.
func strictDecode(data []byte, value any) error {
	if len(data) == 0 || len(data) > MaxResponseBytes || !utf8.Valid(data) || !validEscapes(data) {
		return ErrOutput
	}
	tokens := json.NewDecoder(bytes.NewReader(data))
	tokens.UseNumber()
	if walk(tokens, 0) != nil {
		return ErrOutput
	}
	if _, err := tokens.Token(); !errors.Is(err, io.EOF) {
		return ErrOutput
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return ErrOutput
	}
	return nil
}
func walk(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrOutput
	}
	token, err := d.Token()
	if err != nil {
		return ErrOutput
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return ErrOutput
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrOutput
			}
			seen[name] = true
			if walk(d, depth+1) != nil {
				return ErrOutput
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrOutput
		}
	case '[':
		for d.More() {
			if walk(d, depth+1) != nil {
				return ErrOutput
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrOutput
		}
	default:
		return ErrOutput
	}
	return nil
}
func validEscapes(data []byte) bool {
	inside := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || data[i] != '\\' {
			continue
		}
		if i+1 >= len(data) {
			return false
		}
		if data[i+1] != 'u' {
			i++
			continue
		}
		if i+6 > len(data) {
			return false
		}
		n, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
		if err != nil {
			return false
		}
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+12 > len(data) || data[i+6] != '\\' || data[i+7] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+8:i+12]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 11
		} else {
			i += 5
		}
	}
	return !inside
}
