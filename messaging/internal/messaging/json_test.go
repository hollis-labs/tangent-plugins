package messaging

import "testing"

func TestStrictJSONRejectsAmbiguousBodyAndUnicodeReplacement(t *testing.T) {
	for _, raw := range []string{`{"text":"first","text":"last"}`, `{"text":"\ud800"}`, `{"text":"\udc00"}`, `{"text":"\ud800\u0020"}`, `{"text":"valid"} {}`, "{\"text\":\"\xff\"}"} {
		if strictJSON([]byte(raw)) {
			t.Fatal("ambiguous body accepted")
		}
		message := routedMessage()
		message.Payload = []byte(raw)
		if _, err := Admit(testSource(), message); err == nil {
			t.Fatal("publication did not refuse invalid body")
		}
	}
	for _, raw := range []string{`{"text":"\ud83d\ude00"}`, `{"text":"\\ud800"}`, `"normal 世界"`} {
		if !strictJSON([]byte(raw)) {
			t.Fatal("valid Unicode refused")
		}
	}
}
