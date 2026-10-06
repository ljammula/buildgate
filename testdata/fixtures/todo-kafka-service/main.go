package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"todo-service/cache"
	"todo-service/event"
	"todo-service/pgstore"
)

type Todo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

type Store struct {
	mu    sync.RWMutex
	todos []Todo
}

func NewStore() *Store {
	return &Store{todos: []Todo{}}
}

func (s *Store) List() []Todo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Todo, len(s.todos))
	copy(out, s.todos)
	return out
}

// AddWithID inserts a todo under an id the caller already generated (see
// newTodoID), rather than assigning one here, so the same id can be used
// both for this immediate in-memory response and for the Kafka event a
// worker later writes into Postgres under.
func (s *Store) AddWithID(id, title string) Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := Todo{ID: id, Title: title}
	s.todos = append(s.todos, t)
	return t
}

func (s *Store) Get(id string) (Todo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.todos {
		if t.ID == id {
			return t, true
		}
	}
	return Todo{}, false
}

func (s *Store) Update(id string, title string, done bool) (Todo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.todos {
		if t.ID == id {
			t.Title = title
			t.Done = done
			s.todos[i] = t
			return t, true
		}
	}
	return Todo{}, false
}

func (s *Store) Patch(id string, title *string, done *bool) (Todo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.todos {
		if t.ID == id {
			if title != nil {
				t.Title = *title
			}
			if done != nil {
				t.Done = *done
			}
			s.todos[i] = t
			return t, true
		}
	}
	return Todo{}, false
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.todos {
		if t.ID == id {
			s.todos = append(s.todos[:i], s.todos[i+1:]...)
			return true
		}
	}
	return false
}

var store = NewStore()

// DurableLister is the seam between handleTodosDurable and the real
// pgstore.Store, so main_test.go's unit tests never need a live Postgres
// -- the same injectable-dependency shape publisher uses in
// publisher.go. pgstore.Store already satisfies this shape.
type DurableLister interface {
	List() ([]pgstore.Todo, error)
}

// durableStore is nil by default so `go test ./...` and any run without
// TODO_DATABASE_URL set don't require a reachable Postgres -- main wires
// in a real pgstore.Store only when TODO_DATABASE_URL is set.
var durableStore DurableLister

func handleTodos(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/todos")
	if path != "" {
		path = strings.TrimPrefix(path, "/")
	}

	switch r.Method {
	case http.MethodGet:
		if path == "" || path == "/" {
			jsonNewline(store.List(), w)
			return
		}
		if t, ok := store.Get(path); ok {
			jsonNewline(t, w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return

	case http.MethodPost:
		var body struct{ Title string }
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Title == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		// Publishing and key state both outlive a client disconnect: a
		// publish cancelled after the broker already took the event would
		// release the key and let the retry publish it again, and a
		// cancelled Complete would leave the claim looking in flight.
		// publishTimeout keeps the publish inside the key's claim TTL.
		keyCtx := context.WithoutCancel(r.Context())
		if len(key) > maxIdempotencyKeyLen {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if key != "" {
			prior, err := idempotency.Claim(r.Context(), key, body.Title)
			if err != nil {
				// Without the lookup there is no way to tell a retry from
				// a new request, so refuse rather than risk a duplicate.
				log.Printf("claim idempotency key: %v", err)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			switch {
			case prior == nil:
				// Claimed: fall through and create the todo.
			case prior.Title != body.Title:
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			case prior.Todo == nil:
				w.WriteHeader(http.StatusConflict)
				return
			default:
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(http.StatusCreated)
				jsonNewline(*prior.Todo, w)
				return
			}
		}

		// Publish first, cache second: the in-memory Store below is only
		// this process's own read cache for an immediate response --
		// Postgres, materialized by cmd/consumer off this same event, is
		// the durable store. A publish failure must not report success
		// with nothing behind it, so the store is only updated once the
		// event is confirmed queued.
		id := newTodoID()
		publishCtx, cancel := context.WithTimeout(keyCtx, publishTimeout)
		err := publisher.Publish(publishCtx, event.TodoCreated{ID: id, Title: body.Title, CreatedAt: time.Now().UTC()})
		cancel()
		if err != nil {
			log.Printf("publish todo.created for %s: %v", id, err)
			if key != "" {
				// Nothing was created, so the client's retry must be
				// allowed to try again under the same key.
				if err := idempotency.Release(keyCtx, key); err != nil {
					log.Printf("release idempotency key: %v", err)
				}
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		t := store.AddWithID(id, body.Title)
		if key != "" {
			// The event is already published, so a failure here is logged,
			// not returned: an error response would invite the very retry
			// this key exists to absorb.
			if err := idempotency.Complete(keyCtx, key, idempotencyRecord{Title: body.Title, Todo: &t}); err != nil {
				log.Printf("complete idempotency key for %s: %v", id, err)
			}
		}
		w.WriteHeader(http.StatusCreated)
		jsonNewline(t, w)
		return

	case http.MethodPut:
		if path == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			Title string `json:"title"`
			Done  bool   `json:"done"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Title == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if t, ok := store.Update(path, body.Title, body.Done); ok {
			jsonNewline(t, w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return

	case http.MethodPatch:
		if path == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			Title *string `json:"title"`
			Done  *bool   `json:"done"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Title == nil && body.Done == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Title != nil && *body.Title == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if t, ok := store.Patch(path, body.Title, body.Done); ok {
			jsonNewline(t, w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return

	case http.MethodDelete:
		if path == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if store.Delete(path) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type durableStats struct {
	Total int `json:"total"`
	Done  int `json:"done"`
}

// readDurable centralizes the durable list read and its error responses. Both
// durable endpoints therefore use exactly the same cache/Postgres path.
func readDurable(w http.ResponseWriter) ([]pgstore.Todo, bool) {
	if durableStore == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return nil, false
	}
	todos, err := durableStore.List()
	if err != nil {
		log.Printf("list durable todos: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return nil, false
	}
	return todos, true
}

func handleTodosDurable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// An invalid done filter is a 400 whether or not the durable store is
	// configured, so it is validated before the read.
	values, hasDone := r.URL.Query()["done"]
	var done bool
	if hasDone {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			http.Error(w, "done must be true or false", http.StatusBadRequest)
			return
		}
		done = values[0] == "true"
	}
	todos, ok := readDurable(w)
	if !ok {
		return
	}
	if hasDone {
		filtered := make([]pgstore.Todo, 0, len(todos))
		for _, todo := range todos {
			if todo.Done == done {
				filtered = append(filtered, todo)
			}
		}
		todos = filtered
	}
	jsonNewline(todos, w)
}

func handleTodosDurableStats(w http.ResponseWriter, r *http.Request) {
	// Reject the method before looking at durableStore: even an unavailable
	// store must not turn a non-GET into a durable read.
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	todos, ok := readDurable(w)
	if !ok {
		return
	}
	stats := durableStats{Total: len(todos)}
	for _, todo := range todos {
		if todo.Done {
			stats.Done++
		}
	}
	jsonNewline(stats, w)
}

func registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/todos", handleTodos)
	mux.HandleFunc("/todos/", handleTodos)
	mux.HandleFunc("/todos/durable", handleTodosDurable)
	mux.HandleFunc("/todos/durable/stats", handleTodosDurableStats)
}

func jsonNewline(v any, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// publishTimeout caps one todo.created publish. It must stay well under
// idempotencyClaimTTL: a publish still running when its claim expires
// lets a retry claim the key and publish a second todo.
const publishTimeout = 10 * time.Second

// durableListCacheTTL bounds how stale GET /todos/durable can be -- see
// cache.ListCache for when it is stale at all.
const durableListCacheTTL = 30 * time.Second

func main() {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		log.Fatal("REDIS_ADDR is required (e.g. localhost:6380)")
	}
	rdb, err := cache.Connect(redisAddr)
	if err != nil {
		log.Fatalf("connect to redis: %v", err)
	}
	defer rdb.Close()
	idempotency = redisIdempotencyStore{rdb: rdb}
	log.Printf("redis connected at %s", redisAddr)

	// publisher stays the noopPublisher default (see publisher.go) unless
	// KAFKA_BROKERS is set -- so running this binary with no Kafka
	// configured behaves exactly as it did before Kafka was added.
	if brokers := os.Getenv("KAFKA_BROKERS"); brokers != "" {
		topic := os.Getenv("KAFKA_TOPIC")
		if topic == "" {
			topic = event.TodosCreatedTopic
		}
		kp := NewKafkaPublisher(strings.Split(brokers, ","), topic)
		defer kp.Close()
		publisher = kp
		log.Printf("publishing %s to kafka brokers %s, topic %q", event.TodosCreatedTopic, brokers, topic)
	}

	// durableStore stays nil (see DurableLister above) unless
	// TODO_DATABASE_URL is set -- a missing or unreachable Postgres must
	// not prevent the API from serving its existing endpoints, so a
	// connect failure only logs and continues, mirroring how the Kafka
	// wiring above simply does nothing when KAFKA_BROKERS is unset.
	if dsn := os.Getenv("TODO_DATABASE_URL"); dsn != "" {
		ds, err := pgstore.New(dsn)
		if err != nil {
			log.Printf("connect durable store: %v -- /todos/durable will be unavailable", err)
		} else {
			defer ds.Close()
			durableStore = cache.NewListCache(rdb, ds, durableListCacheTTL)
			log.Printf("durable store connected, cached in redis for %s", durableListCacheTTL)
		}
	}

	registerRoutes(http.DefaultServeMux)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("listening on :%s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("listen on :%s: %v", port, err)
	}
}
