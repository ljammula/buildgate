package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"todo-service/pgstore"
)

func handler(path string, method string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/todos"+path, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleTodos(w, req)
	return w
}

func resetStore() {
	store = NewStore()
}

func TestListEmpty(t *testing.T) {
	resetStore()
	w := handler("", "GET", "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var todos []Todo
	json.NewDecoder(w.Body).Decode(&todos)
	if len(todos) != 0 {
		t.Fatalf("expected empty list, got %d items", len(todos))
	}
}

func TestAddAndGet(t *testing.T) {
	resetStore()
	w := handler("", "POST", `{"title":"buy milk"}`)
	if w.Code != 201 {
		t.Fatalf("POST expected 201, got %d", w.Code)
	}
	var todo Todo
	json.NewDecoder(w.Body).Decode(&todo)
	if todo.Title != "buy milk" {
		t.Fatal("wrong title")
	}
	if todo.ID == "" {
		t.Fatal("missing id")
	}

	w2 := handler("/"+todo.ID, "GET", "")
	if w2.Code != 200 {
		t.Fatalf("GET expected 200, got %d", w2.Code)
	}
	var got Todo
	json.NewDecoder(w2.Body).Decode(&got)
	if got.Title != "buy milk" {
		t.Fatal("wrong title on GET")
	}
}

func TestUpdate(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		initial    Todo
		wantStatus int
		wantTodo   Todo
	}{
		{
			name:       "PUT omits title",
			method:     http.MethodPut,
			body:       `{"done":true}`,
			initial:    Todo{ID: "put-omitted", Title: "old"},
			wantStatus: http.StatusBadRequest,
			wantTodo:   Todo{ID: "put-omitted", Title: "old"},
		},
		{
			name:       "PUT empty title",
			method:     http.MethodPut,
			body:       `{"title":"","done":true}`,
			initial:    Todo{ID: "put-empty", Title: "old"},
			wantStatus: http.StatusBadRequest,
			wantTodo:   Todo{ID: "put-empty", Title: "old"},
		},
		{
			name:       "PATCH empty title",
			method:     http.MethodPatch,
			body:       `{"title":""}`,
			initial:    Todo{ID: "patch-empty", Title: "old", Done: true},
			wantStatus: http.StatusBadRequest,
			wantTodo:   Todo{ID: "patch-empty", Title: "old", Done: true},
		},
		{
			name:       "valid PUT",
			method:     http.MethodPut,
			body:       `{"title":"new title","done":true}`,
			initial:    Todo{ID: "put-valid", Title: "old"},
			wantStatus: http.StatusOK,
			wantTodo:   Todo{ID: "put-valid", Title: "new title", Done: true},
		},
		{
			name:       "valid PATCH preserves omitted fields",
			method:     http.MethodPatch,
			body:       `{"title":"new title"}`,
			initial:    Todo{ID: "patch-valid", Title: "old", Done: true},
			wantStatus: http.StatusOK,
			wantTodo:   Todo{ID: "patch-valid", Title: "new title", Done: true},
		},
		{
			name:       "done-only PATCH preserves title",
			method:     http.MethodPatch,
			body:       `{"done":true}`,
			initial:    Todo{ID: "patch-done-only", Title: "old"},
			wantStatus: http.StatusOK,
			wantTodo:   Todo{ID: "patch-done-only", Title: "old", Done: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetStore()
			store.todos = []Todo{tt.initial}

			w := handler("/"+tt.initial.ID, tt.method, tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, w.Code)
			}
			if tt.wantStatus == http.StatusBadRequest && w.Body.Len() != 0 {
				t.Fatalf("expected empty response body, got %q", w.Body.String())
			}
			if tt.wantStatus == http.StatusOK {
				var got Todo
				if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if got != tt.wantTodo {
					t.Fatalf("response todo = %+v, want %+v", got, tt.wantTodo)
				}
			}

			got, ok := store.Get(tt.initial.ID)
			if !ok {
				t.Fatal("todo was removed")
			}
			if got != tt.wantTodo {
				t.Fatalf("stored todo = %+v, want %+v", got, tt.wantTodo)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	resetStore()
	w := handler("", "POST", `{"title":"gone"}`)
	var todo Todo
	json.NewDecoder(w.Body).Decode(&todo)

	w2 := handler("/"+todo.ID, "DELETE", "")
	if w2.Code != 204 {
		t.Fatalf("DELETE expected 204, got %d", w2.Code)
	}

	w3 := handler("/"+todo.ID, "GET", "")
	if w3.Code != 404 {
		t.Fatalf("GET after delete expected 404, got %d", w3.Code)
	}
}

func TestNotFound(t *testing.T) {
	resetStore()
	w := handler("/999", "GET", "")
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestBadPost(t *testing.T) {
	resetStore()
	w := handler("", "POST", `{"title":""}`)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestPatchTitleOnly(t *testing.T) {
	resetStore()
	w := handler("", "POST", `{"title":"old"}`)
	var todo Todo
	json.NewDecoder(w.Body).Decode(&todo)

	w2 := handler("/"+todo.ID, "PATCH", `{"title":"new title"}`)
	if w2.Code != 200 {
		t.Fatalf("PATCH expected 200, got %d", w2.Code)
	}
	var updated Todo
	json.NewDecoder(w2.Body).Decode(&updated)
	if updated.Title != "new title" {
		t.Fatalf("expected new title, got %q", updated.Title)
	}
	if updated.Done {
		t.Fatal("done should have stayed false")
	}

	w3 := handler("/"+todo.ID, "PATCH", `{"done":true}`)
	if w3.Code != 200 {
		t.Fatalf("PATCH expected 200, got %d", w3.Code)
	}
	var updated2 Todo
	json.NewDecoder(w3.Body).Decode(&updated2)
	if !updated2.Done {
		t.Fatal("expected done=true after patch")
	}
	if updated2.Title != "new title" {
		t.Fatalf("title should have stayed %q, got %q", "new title", updated2.Title)
	}
}

func TestPatchNonexistent(t *testing.T) {
	resetStore()
	w := handler("/999", "PATCH", `{"title":"x"}`)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPatchEmptyBody(t *testing.T) {
	resetStore()
	w := handler("", "POST", `{"title":"old"}`)
	var todo Todo
	json.NewDecoder(w.Body).Decode(&todo)

	w2 := handler("/"+todo.ID, "PATCH", `{}`)
	if w2.Code != 400 {
		t.Fatalf("PATCH {} expected 400, got %d", w2.Code)
	}

	w3 := handler("", "PATCH", `{"title":"x"}`)
	if w3.Code != 400 {
		t.Fatalf("PATCH empty path expected 400, got %d", w3.Code)
	}
}

// fakeDurable is a DurableLister for unit tests -- no live Postgres.
type fakeDurable struct {
	todos []pgstore.Todo
	err   error
}

func (f fakeDurable) List() ([]pgstore.Todo, error) { return f.todos, f.err }

type countingDurable struct {
	calls int
}

func (f *countingDurable) List() ([]pgstore.Todo, error) {
	f.calls++
	return nil, nil
}

func durableRequest(method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	w := httptest.NewRecorder()
	handleTodosDurable(w, req)
	return w
}

func TestTodosDurableFilters(t *testing.T) {
	prev := durableStore
	durableStore = fakeDurable{todos: []pgstore.Todo{
		{ID: "first", Title: "open", Done: false},
		{ID: "second", Title: "closed", Done: true},
		{ID: "third", Title: "also closed", Done: true},
	}}
	defer func() { durableStore = prev }()

	for _, test := range []struct {
		query string
		want  []string
	}{
		{"", []string{"first", "second", "third"}},
		{"?done=true", []string{"second", "third"}},
		{"?done=false", []string{"first"}},
	} {
		w := durableRequest(http.MethodGet, "/todos/durable"+test.query)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", test.query, w.Code)
		}
		var got []pgstore.Todo
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", test.query, err)
		}
		if len(got) != len(test.want) {
			t.Fatalf("%s: got %d todos, want %d: %+v", test.query, len(got), len(test.want), got)
		}
		for i, id := range test.want {
			if got[i].ID != id {
				t.Fatalf("%s: item %d is %q, want %q", test.query, i, got[i].ID, id)
			}
		}
	}
}

func TestTodosDurableEmptyFilterIsArray(t *testing.T) {
	prev := durableStore
	durableStore = fakeDurable{todos: []pgstore.Todo{{ID: "open"}}}
	defer func() { durableStore = prev }()

	w := durableRequest(http.MethodGet, "/todos/durable?done=true")
	if w.Code != http.StatusOK || w.Body.String() != "[]\n" {
		t.Fatalf("expected empty JSON array, got status %d body %q", w.Code, w.Body.String())
	}
}

func TestTodosDurableRejectsInvalidDoneWithoutReading(t *testing.T) {
	for _, query := range []string{"?done=", "?done=TRUE", "?done=1", "?done=bogus"} {
		lister := &countingDurable{}
		prev := durableStore
		durableStore = lister
		w := durableRequest(http.MethodGet, "/todos/durable"+query)
		durableStore = prev
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", query, w.Code)
		}
		if !strings.Contains(w.Body.String(), "done must be true or false") {
			t.Errorf("%s: unexpected error %q", query, w.Body.String())
		}
		if lister.calls != 0 {
			t.Errorf("%s: durable store was read %d times", query, lister.calls)
		}
	}
}

func TestTodosDurableList(t *testing.T) {
	resetStore()
	prev := durableStore
	durableStore = fakeDurable{todos: []pgstore.Todo{{ID: "1", Title: "a"}, {ID: "2", Title: "b", Done: true}}}
	defer func() { durableStore = prev }()

	req := httptest.NewRequest(http.MethodGet, "/todos/durable", nil)
	w := httptest.NewRecorder()
	handleTodosDurable(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var todos []pgstore.Todo
	if err := json.NewDecoder(w.Body).Decode(&todos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(todos) != 2 || todos[0].ID != "1" || todos[0].Title != "a" || todos[1].ID != "2" || todos[1].Title != "b" || !todos[1].Done {
		t.Fatalf("unexpected todos: %+v", todos)
	}
}

func TestTodosDurableNilStore(t *testing.T) {
	prev := durableStore
	durableStore = nil
	defer func() { durableStore = prev }()

	req := httptest.NewRequest(http.MethodGet, "/todos/durable", nil)
	w := httptest.NewRecorder()
	handleTodosDurable(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestTodosDurableListError(t *testing.T) {
	prev := durableStore
	durableStore = fakeDurable{err: errors.New("boom")}
	defer func() { durableStore = prev }()

	req := httptest.NewRequest(http.MethodGet, "/todos/durable", nil)
	w := httptest.NewRecorder()
	handleTodosDurable(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestTodosDurableMethodNotAllowed(t *testing.T) {
	prev := durableStore
	durableStore = nil
	defer func() { durableStore = prev }()

	req := httptest.NewRequest(http.MethodPost, "/todos/durable", nil)
	w := httptest.NewRecorder()
	handleTodosDurable(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

type countingStatsDurable struct {
	todos []pgstore.Todo
	calls *int
}

func (f countingStatsDurable) List() ([]pgstore.Todo, error) {
	(*f.calls)++
	return f.todos, nil
}

func TestTodosDurableStats(t *testing.T) {
	prev := durableStore
	defer func() { durableStore = prev }()

	tests := []struct {
		name  string
		todos []pgstore.Todo
		total int
		done  int
	}{
		{"mixed", []pgstore.Todo{{Done: true}, {Done: false}, {Done: true}}, 3, 2},
		{"empty", []pgstore.Todo{}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			durableStore = fakeDurable{todos: tt.todos}
			w := httptest.NewRecorder()
			handleTodosDurableStats(w, httptest.NewRequest(http.MethodGet, "/todos/durable/stats", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			var got map[string]int
			if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got["total"] != tt.total || got["done"] != tt.done {
				t.Fatalf("unexpected stats fields: %#v", got)
			}
		})
	}
}

func TestTodosDurableStatsFailures(t *testing.T) {
	prev := durableStore
	defer func() { durableStore = prev }()

	for _, tc := range []struct {
		name string
		fake DurableLister
		want int
	}{
		{"unavailable", nil, http.StatusServiceUnavailable},
		{"backend error", fakeDurable{err: errors.New("boom")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			durableStore = tc.fake
			w := httptest.NewRecorder()
			handleTodosDurableStats(w, httptest.NewRequest(http.MethodGet, "/todos/durable/stats", nil))
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, w.Code)
			}
		})
	}
}

func TestTodosDurableStatsMethodRejectedBeforeRead(t *testing.T) {
	prev := durableStore
	defer func() { durableStore = prev }()
	calls := 0
	durableStore = countingStatsDurable{calls: &calls}
	w := httptest.NewRecorder()
	handleTodosDurableStats(w, httptest.NewRequest(http.MethodPost, "/todos/durable/stats", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if calls != 0 {
		t.Fatalf("non-GET performed %d durable reads", calls)
	}
}

func TestDurableRoutes(t *testing.T) {
	prev := durableStore
	defer func() { durableStore = prev }()
	durableStore = fakeDurable{todos: []pgstore.Todo{{ID: "1"}}}
	mux := http.NewServeMux()
	registerRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/todos/durable", nil))
	var todos []pgstore.Todo
	if w.Code != http.StatusOK || json.NewDecoder(w.Body).Decode(&todos) != nil || len(todos) != 1 {
		t.Fatalf("durable list route returned %d and %+v", w.Code, todos)
	}
}

func TestListAfterAdd(t *testing.T) {
	resetStore()
	handler("", "POST", `{"title":"a"}`)
	handler("", "POST", `{"title":"b"}`)
	w := handler("", "GET", "")
	var todos []Todo
	json.NewDecoder(w.Body).Decode(&todos)
	if len(todos) != 2 {
		t.Fatalf("expected 2, got %d", len(todos))
	}
}
