package sim

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// MQTTPublisher spreads batches over several broker connections (QoS 1),
// as OEM cloud connectors would.
type MQTTPublisher struct {
	clients []mqtt.Client
	next    atomic.Uint64
	Topic   string // with %s for the OEM code
}

func NewMQTTPublisher(url, clientPrefix, user, pass string, conns int) (*MQTTPublisher, error) {
	p := &MQTTPublisher{Topic: "fleetpulse/oem/%s/telemetry"}
	for i := 0; i < conns; i++ {
		o := mqtt.NewClientOptions().AddBroker(url).SetClientID(fmt.Sprintf("%s-%d-%d", clientPrefix, time.Now().UnixNano()%100000, i)).
			SetAutoReconnect(true).SetConnectRetry(true).SetConnectRetryInterval(2 * time.Second).
			SetMaxResumePubInFlight(1000).SetWriteTimeout(10 * time.Second)
		if user != "" {
			o.SetUsername(user).SetPassword(pass)
		}
		c := mqtt.NewClient(o)
		t := c.Connect()
		if !t.WaitTimeout(30*time.Second) || t.Error() != nil {
			return nil, fmt.Errorf("mqtt connect: %v", t.Error())
		}
		p.clients = append(p.clients, c)
	}
	return p, nil
}

func (p *MQTTPublisher) Publish(ctx context.Context, oem string, payload []byte) error {
	c := p.clients[p.next.Add(1)%uint64(len(p.clients))]
	t := c.Publish(fmt.Sprintf(p.Topic, oem), 1, false, payload)
	select {
	case <-t.Done():
		return t.Error()
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
		return fmt.Errorf("mqtt publish timeout")
	}
}

func (p *MQTTPublisher) Close() {
	for _, c := range p.clients {
		c.Disconnect(500)
	}
}

// HTTPPublisher posts gzip-compressed batches to the ingest gateway, the
// path OEM clouds use for webhook-style delivery. 429/503 surface as errors
// so the runner backs off (back-pressure end to end).
type HTTPPublisher struct {
	BaseURL string
	Keys    map[string]string
	Client  *http.Client
}

func NewHTTPPublisher(base, apiKeys string) *HTTPPublisher {
	keys := map[string]string{}
	for _, kv := range strings.Split(apiKeys, ",") {
		if parts := strings.SplitN(strings.TrimSpace(kv), ":", 2); len(parts) == 2 {
			keys[parts[0]] = parts[1]
		}
	}
	return &HTTPPublisher{BaseURL: strings.TrimRight(base, "/"), Keys: keys, Client: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 64, MaxConnsPerHost: 128},
	}}
}

func (h *HTTPPublisher) Publish(ctx context.Context, oem string, payload []byte) error {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+"/v1/ingest/"+oem, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Api-Key", h.Keys[oem])
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		return nil // malformed data was dead-lettered; retrying will not help
	default:
		return fmt.Errorf("ingest returned %d", resp.StatusCode)
	}
}
