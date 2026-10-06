package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"

	"todo-service/event"
)

// KafkaPublisher is the real EventPublisher (see publisher.go), backed by
// a Kafka producer against the broker docker-compose.yml's own "kafka"
// service runs.
type KafkaPublisher struct {
	writer  *kafka.Writer
	brokers []string
	topic   string
}

func NewKafkaPublisher(brokers []string, topic string) *KafkaPublisher {
	return &KafkaPublisher{
		writer: &kafka.Writer{
			Addr:         kafka.TCP(brokers...),
			Topic:        topic,
			Balancer:     &kafka.LeastBytes{},
			RequiredAcks: kafka.RequireOne,
		},
		brokers: brokers,
		topic:   topic,
	}
}

// Publish creates the topic on demand: the writer never creates topics,
// so on a fresh broker the first write fails with UnknownTopicOrPartition
// until something else (cmd/consumer joining its group) happens to create
// it -- an API started first rejected every POST. The writer's
// AllowAutoTopicCreation is no substitute: the broker still rejects the
// write that triggers it. Creating here rather than once at startup also
// covers a broker that wasn't reachable yet when the API started.
//
// After creating, the write is retried until it lands or ctx expires: the
// writer's transport caches cluster metadata (6s by default), so it keeps
// reporting the topic unknown until that cache refreshes. Only the first
// publish to a fresh broker pays this.
func (p *KafkaPublisher) Publish(ctx context.Context, evt event.TodoCreated) error {
	body, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	// Keyed by id so any future partitioning still routes every event for
	// one todo through the same partition, preserving per-todo ordering.
	msg := kafka.Message{Key: []byte(evt.ID), Value: body}
	err = p.writer.WriteMessages(ctx, msg)
	if !isUnknownTopic(err) {
		return err
	}
	if cerr := createTopic(ctx, p.brokers, p.topic); cerr != nil {
		return fmt.Errorf("%w (creating topic %q: %v)", err, p.topic, cerr)
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (topic %q created, not yet visible: %v)", err, p.topic, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
		err = p.writer.WriteMessages(ctx, msg)
		if !isUnknownTopic(err) {
			return err
		}
	}
}

func (p *KafkaPublisher) Close() error { return p.writer.Close() }

// isUnknownTopic reports whether err is UnknownTopicOrPartition, returned
// directly or per message inside WriteErrors.
func isUnknownTopic(err error) bool {
	if errors.Is(err, kafka.UnknownTopicOrPartition) {
		return true
	}
	var werrs kafka.WriteErrors
	if errors.As(err, &werrs) {
		for _, e := range werrs {
			if errors.Is(e, kafka.UnknownTopicOrPartition) {
				return true
			}
		}
	}
	return false
}

// createTopic creates topic (1 partition, replication 1) through the
// cluster controller, trying each broker in turn, and succeeds if the
// topic already exists. ctx's deadline also bounds every broker round
// trip: kafka-go's Conn otherwise waits on a silent broker forever.
func createTopic(ctx context.Context, brokers []string, topic string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	var lastErr error
	for _, broker := range brokers {
		if lastErr = createTopicVia(ctx, broker, topic, deadline); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func createTopicVia(ctx context.Context, broker, topic string, deadline time.Time) error {
	conn, err := kafka.DialContext(ctx, "tcp", broker)
	if err != nil {
		return fmt.Errorf("dial %s: %w", broker, err)
	}
	defer conn.Close()
	conn.SetDeadline(deadline)
	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("find controller via %s: %w", broker, err)
	}
	cconn, err := kafka.DialContext(ctx, "tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return fmt.Errorf("dial controller: %w", err)
	}
	defer cconn.Close()
	cconn.SetDeadline(deadline)
	return cconn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	})
}
