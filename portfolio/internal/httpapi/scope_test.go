package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestHTTPScopedEnvelopeStrictDTOAndUnchangedExactJSON(t *testing.T) {
	s := fixtureService(t)
	a := New(s, fixtureVerifier)
	out := resultObject(t, a, "list", `{"db":"ideas","scope":{"kind":"unscoped"},"page":{"limit":1}}`, http.StatusOK)
	if out["total"] != json.Number("2") || len(out["membership"].(map[string]any)) != 1 {
		t.Fatal(out)
	}
	row := out["items"].([]any)[0].(map[string]any)
	if row["unknown"].(map[string]any)["number"] != json.Number("9007199254740993") {
		t.Fatal("exact HTTP source number changed", row)
	}
	for _, body := range []string{
		`{"db":"ideas","scope":null}`,
		`{"db":"ideas","scope":{"kind":"unscoped","role":"owner"}}`,
		`{"db":"ideas","scope":{"kind":"unscoped","kind":"project"}}`,
	} {
		assertError(t, resultObject(t, a, "list", body, http.StatusBadRequest), "bad_request")
	}
	assertError(t, resultObject(t, a, "list", `{"db":"ideas","scope":{"kind":"workstream","id":"WS-missing"}}`, http.StatusNotFound), "not_found")
	assertError(t, resultObject(t, New(s, nil), "list", `{"db":"ideas","scope":{"kind":"unscoped"}}`, http.StatusServiceUnavailable), "unavailable")
}
