# todo-service

A small Go todo API whose writes flow through Kafka into Postgres, with Redis
as cache and idempotency store. Two processes: the API (`main.go`) and the
consumer (`cmd/consumer`).

## Flow

```text
 client
   |  POST /todos  {"title": ...}   [Idempotency-Key: k]
   v
+-------------------------- API (main.go) ---------------------------+
|  1. key given?  Redis SET NX idempotency:post-todos:k  (30s)       |
|       taken, done      -> 201 + original todo (Idempotent-Replayed)|
|       taken, in flight -> 409      other title -> 422              |
|       Redis error      -> 503                                      |
|  2. publish TodoCreated{id,title,created_at}, 10s cap ------+      |
|       fails -> release key, 502                             |      |
|  3. add to in-memory store, record todo under key (24h)     |      |
|  4. 201 {"id","title","done"}                               |      |
+-------------------------------------------------------------|------+
                                                              v
                                               Kafka topic todos.created
                                                              |
+------------------------ consumer (cmd/consumer) ------------|------+
|  5. fetch message  <----------------------------------------+      |
|  6. INSERT (event's created_at) ON CONFLICT (id) DO NOTHING        |
|       fails -> offset not committed, redelivered later             |
|  7. Redis DEL todos:durable:list   (failure only logged)           |
|  8. commit offset                                                  |
+--------------------------------------------------------------------+

 client
   |  GET /todos/durable
   v
 API: Redis GET todos:durable:list          (each Redis call capped at 100ms)
        hit  -> cached JSON
        miss -> Postgres SELECT ... ORDER BY created_at, id
                -> Redis SET todos:durable:list (30s) -> JSON
        Redis error -> read Postgres, no write-back
```

Startup, both processes:

```text
REDIS_ADDR set and PING ok? --no--> exit 1
            | yes
pgstore.New: pg_advisory_xact_lock -> apply pending pgstore/migrations/*.sql
             -> record each in schema_migrations   (one transaction)
```

## Services

| Service | Role | Local port | Required by |
|---|---|---|---|
| Postgres 16 | Durable todos (`pgstore`) | 5433 | consumer; API for `/todos/durable` |
| Kafka 3.8 (KRaft) | `todos.created` events | 9094 | consumer; API publishes when set |
| Redis 7.4 | Idempotency keys, durable-list cache | 6380 | API and consumer |

## Configuration

| Variable | Used by | Default |
|---|---|---|
| `REDIS_ADDR` | API, consumer | required |
| `TODO_DATABASE_URL` | consumer (required), API (enables `/todos/durable`) | unset |
| `KAFKA_BROKERS` | consumer, API (unset: events not published) | consumer: `localhost:9094` |
| `KAFKA_TOPIC` | API, consumer | `todos.created` |
| `PORT` | API | `8080` |

## Endpoints

| Method and path | Behaviour |
|---|---|
| `GET /todos`, `GET /todos/{id}` | This API process's in-memory todos |
| `POST /todos` | Publish `todos.created`; optional `Idempotency-Key` header (max 255 chars) |
| `PUT`, `PATCH`, `DELETE /todos/{id}` | In-memory todos only |
| `GET /todos/durable` | Postgres todos, oldest `created_at` first (stamped by the API, not the consumer), cached in Redis for 30s |

## Migrations

`pgstore/migrations/NNN_description.sql`, versions 1..N with no gaps, applied
in order by `pgstore.New`. Add a migration as the next number; never edit
one that has shipped.

## Verify

| Command | Needs |
|---|---|
| `make test` | nothing (unit tests) |
| `make verify` | Docker; starts and removes the compose stack |
| `make verify-integration` | Postgres, Kafka and Redis already running (`BG_SERVICE_*` or `PG_ADDR`/`KAFKA_BROKERS`/`REDIS_ADDR`); buildgate's `verify_command` |
