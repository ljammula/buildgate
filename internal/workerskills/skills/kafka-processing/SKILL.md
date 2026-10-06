---
name: kafka-processing
description: Kafka producer, consumer, stream and retry work in a buildgate build round. Use only when the repository has a Kafka client dependency or Kafka configuration and the ticket touches it.
---

# Kafka processing (buildgate)


State the delivery contract the changed code relies on: at-most-once, at-least-once with idempotency, or Kafka-scoped exactly-once. Do not claim exactly-once across an external database from Kafka transactions alone.

For the paths the ticket changes, test duplicate delivery, retry exhaustion and poison records, and crash before or after the offset commit where the code commits offsets. When the round names a Kafka sidecar (`BG_SERVICE_*` variables), integration tests may use it; otherwise use the repository's in-process test doubles.
