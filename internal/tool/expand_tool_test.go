package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeExpandStore is a minimal ExpandStore for unit-testing the tool layer.
type fakeExpandStore struct {
	originals map[string]string
	getCalled bool
}

func (s *fakeExpandStore) Get(id string) (string, bool) {
	s.getCalled = true
	text, ok := s.originals[id]
	return text, ok
}

func TestExpandTool_Metadata(t *testing.T) {
	e := ExpandTool{Store: &fakeExpandStore{}}
	if e.Name() != "expand" {
		t.Errorf("Name() = %q, want expand", e.Name())
	}
	if e.RequiresConfirmation(nil) {
		t.Error("RequiresConfirmation() = true, want false (read-only lookup)")
	}
	if !strings.Contains(e.Description(), "ORIGINAL") {
		t.Errorf("Description() should advertise original retrieval: %q", e.Description())
	}
	var params map[string]any
	if err := json.Unmarshal(e.Parameters(), &params); err != nil {
		t.Fatalf("Parameters() is not valid JSON: %v", err)
	}
	if params["type"] != "object" {
		t.Errorf("Parameters() type = %v, want object", params["type"])
	}
}

func TestExpandTool_Execute(t *testing.T) {
	store := &fakeExpandStore{originals: map[string]string{
		"elide-3": "the original segment text\n\n",
	}}
	e := ExpandTool{Store: store}

	// Known id → the stored original, byte-for-byte.
	got, err := e.Execute(context.Background(), []byte(`{"id":"elide-3"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got != "the original segment text\n\n" {
		t.Errorf("Execute() = %q, want the stored original", got)
	}
	if !store.getCalled {
		t.Error("Execute() never consulted the store")
	}

	// Surrounding whitespace on the id is tolerated.
	if _, err := e.Execute(context.Background(), []byte(`{"id":" elide-3 "}`)); err != nil {
		t.Errorf("Execute() with padded id error = %v", err)
	}

	// Unknown id → the documented error result.
	_, err = e.Execute(context.Background(), []byte(`{"id":"elide-999"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown elided id") {
		t.Errorf("Execute(unknown id) error = %v, want the unknown-elided-id error", err)
	}

	// Missing id → a required-parameter error.
	_, err = e.Execute(context.Background(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Errorf("Execute(missing id) error = %v, want the required-id error", err)
	}

	// Malformed arguments surface as invalid parameters.
	_, err = e.Execute(context.Background(), []byte(`not json`))
	if err == nil || !strings.Contains(err.Error(), "invalid parameters") {
		t.Errorf("Execute(bad json) error = %v, want an invalid-parameters error", err)
	}
}

// TestExpandTool_StoreWithoutEntries: a tool pointing at an empty store must
// not panic; it reports the id as unknown.
func TestExpandTool_StoreWithoutEntries(t *testing.T) {
	e := ExpandTool{Store: &fakeExpandStore{}}
	_, err := e.Execute(context.Background(), []byte(`{"id":"elide-1"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown elided id") {
		t.Errorf("Execute() error = %v, want the unknown-elided-id error", err)
	}
}

func TestExpandTool_CallString(t *testing.T) {
	e := ExpandTool{Store: &fakeExpandStore{}}
	if got := e.CallString([]byte(`{"id":"elide-3"}`)); !strings.Contains(got, "elide-3") {
		t.Errorf("CallString() = %q, want it to name the id", got)
	}
}
