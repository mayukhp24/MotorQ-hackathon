package ingest

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	json "github.com/goccy/go-json"
	"github.com/twmb/franz-go/pkg/kgo"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/oem"
	"fleetpulse/pipeline/internal/platform"
)

// KafkaSink produces canonical events keyed by VIN (per-vehicle ordering)
// and rejects to the DLQ. Produce returns only after the broker has
// acknowledged every record (acks=all), so a 202 / PUBACK means durable.
type KafkaSink struct {
	Client      *kgo.Client
	MaxBuffered int
	errors      atomic.Int64
}

func (k *KafkaSink) Pressure() float64 {
	if k.MaxBuffered <= 0 {
		return 0
	}
	return float64(k.Client.BufferedProduceRecords()) / float64(k.MaxBuffered)
}

func (k *KafkaSink) Produce(ctx context.Context, events []canonical.Event, rejects []oem.Reject) error {
	var wg sync.WaitGroup
	var firstErr atomic.Value
	cb := func(_ *kgo.Record, err error) {
		if err != nil {
			k.errors.Add(1)
			firstErr.CompareAndSwap(nil, err)
		}
		wg.Done()
	}
	for i := range events {
		v, err := json.Marshal(&events[i])
		if err != nil {
			return err
		}
		wg.Add(1)
		k.Client.Produce(ctx, &kgo.Record{Topic: platform.TopicTelemetry, Key: []byte(events[i].VIN), Value: v}, cb)
	}
	for i := range rejects {
		v, _ := json.Marshal(&rejects[i])
		wg.Add(1)
		k.Client.Produce(ctx, &kgo.Record{Topic: platform.TopicDLQ, Key: []byte(rejects[i].OEM), Value: v}, cb)
	}
	wg.Wait()
	if e := firstErr.Load(); e != nil {
		return fmt.Errorf("kafka produce: %w", e.(error))
	}
	return nil
}

// MQTTConfig for the subscriber.
type MQTTConfig struct {
	URL      string
	ClientID string
	Username string
	Password string
	Topic    string      // e.g. $share/ingest/fleetpulse/oem/+/telemetry
	TLS      *tls.Config // ssl:// brokers; carries the client certificate for mTLS
	// Persistent sessions must only be used with stable client IDs: a
	// replica that goes away for good (rollout, scale-in) otherwise leaves an
	// offline member in the shared subscription that keeps receiving - and
	// losing - its share of messages.
	PersistentSession bool
}

// oemFromTopic extracts {oem} from fleetpulse/oem/{oem}/telemetry.
func oemFromTopic(topic string) string {
	parts := strings.Split(topic, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "oem" {
			return parts[i+1]
		}
	}
	return ""
}

// StartMQTT subscribes with a shared subscription so gateway replicas share
// the load. Handlers block while Kafka applies back-pressure, which stalls
// PUBACKs and makes the broker hold (not drop) QoS 1 messages.
func StartMQTT(ctx context.Context, cfg MQTTConfig, g *Gateway, log *slog.Logger) (mqtt.Client, error) {
	opts := mqtt.NewClientOptions().AddBroker(cfg.URL).SetClientID(cfg.ClientID).
		SetCleanSession(!cfg.PersistentSession).SetAutoReconnect(true).SetConnectRetry(true).
		SetConnectRetryInterval(2 * time.Second).SetOrderMatters(false).
		SetKeepAlive(30 * time.Second)
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username).SetPassword(cfg.Password)
	}
	if cfg.TLS != nil {
		opts.SetTLSConfig(cfg.TLS)
	}
	handler := func(_ mqtt.Client, m mqtt.Message) {
		o := oemFromTopic(m.Topic())
		for {
			if err := g.acquire(ctx, time.Second); err == nil {
				break
			}
			mBackpressure.WithLabelValues("mqtt").Inc()
			if ctx.Err() != nil {
				return
			}
		}
		defer g.release()
		if _, err := g.Handle(ctx, o, "mqtt", m.Payload()); err != nil {
			log.Warn("mqtt batch failed", "oem", o, "err", err)
		}
	}
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		tok := c.Subscribe(cfg.Topic, 1, handler)
		tok.Wait()
		if tok.Error() != nil {
			log.Error("mqtt subscribe failed", "err", tok.Error())
			return
		}
		log.Info("mqtt subscribed", "topic", cfg.Topic)
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) { log.Warn("mqtt connection lost", "err", err) })
	c := mqtt.NewClient(opts)
	tok := c.Connect()
	tok.WaitTimeout(10 * time.Second)
	if err := tok.Error(); err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		c.Disconnect(2000)
	}()
	return c, nil
}
