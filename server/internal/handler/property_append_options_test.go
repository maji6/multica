package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPropertyAppendOptionsConcurrent(t *testing.T) {
	property := createTestProperty(t, map[string]any{"name": "Append environment", "type": "select", "config": map[string]any{"options": []map[string]any{{"name": "Existing", "color": "#3b82f6"}}}})
	original := property.Config.Options[0]
	results := make(chan *httptest.ResponseRecorder, 4)
	for _, name := range []string{"dev", "prod", "dev", "PROD"} {
		go func(name string) {
			w := httptest.NewRecorder()
			req := newRequest("PUT", "/api/properties/"+property.ID, map[string]any{"append_options": []map[string]any{{"name": name, "color": "#6b7280"}}})
			testHandler.UpdateProperty(w, withURLParam(req, "id", property.ID))
			results <- w
		}(name)
	}
	for range 4 {
		w := <-results
		if w.Code != http.StatusOK {
			t.Fatalf("append: %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	testHandler.GetProperty(w, withURLParam(newRequest("GET", "/api/properties/"+property.ID, nil), "id", property.ID))
	var updated PropertyResponse
	if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Config.Options) != 3 || updated.Config.Options[0] != original {
		t.Fatalf("options lost or duplicated: %+v", updated.Config.Options)
	}
	req := newRequest("PUT", "/api/properties/"+property.ID, map[string]any{"append_options": []map[string]any{{"name": "another", "color": "#6b7280"}}, "config": map[string]any{"options": []any{}}})
	w = httptest.NewRecorder()
	testHandler.UpdateProperty(w, withURLParam(req, "id", property.ID))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous update: %d %s", w.Code, w.Body.String())
	}
}

func TestGetConfigDeclaresPropertyAppendOptionsSupport(t *testing.T) {
	w := httptest.NewRecorder()
	testHandler.GetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || cfg["property_append_options_supported"] != true {
		t.Fatalf("atomic append capability missing: %d %v", w.Code, cfg)
	}
}
