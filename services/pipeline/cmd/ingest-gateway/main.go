// Command ingest-gateway accepts OEM telemetry over HTTPS and MQTT,
// normalises it to the canonical schema and publishes it to Kafka.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"fleetpulse/pipeline/internal/ingest"
	"fleetpulse/pipeline/internal/oem"
	"fleetpulse/pipeline/internal/platform"
)

func main() {
	log := platform.Logger("ingest-gateway")
	ctx, cancel := platform.SignalContext()
	defer cancel()

	maxBuf := platform.EnvInt("KAFKA_MAX_BUFFERED", 200_000)
	cl, err := platform.NewKafka()
	if err != nil {
		log.Error("kafka client", "err", err)
		os.Exit(1)
	}
	defer cl.Close()
	if err := platform.EnsureTopics(ctx, cl, log); err != nil {
		log.Error("kafka topics", "err", err)
		os.Exit(1)
	}

	reg := oem.DefaultRegistry()
	specDir := platform.Env("ADAPTER_SPEC_DIR", "/etc/fleetpulse/adapters")
	reload := func() {
		if loaded, errs := reg.LoadSpecDir(specDir); len(loaded) > 0 || len(errs) > 0 {
			log.Info("adapter specs loaded", "loaded", loaded, "errors", errs, "registry", reg.List())
		}
	}
	reload()
	// SIGHUP hot-reloads OEM mapping specs: onboarding without downtime.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			reload()
		}
	}()

	sink := &ingest.KafkaSink{Client: cl, MaxBuffered: maxBuf}
	g := ingest.NewGateway(reg, sink, log, platform.EnvInt("INGEST_MAX_INFLIGHT", 256), platform.Env("INGEST_API_KEYS", ""))

	ready := func() error {
		pctx, c := context.WithTimeout(ctx, 2*time.Second)
		defer c()
		return cl.Ping(pctx)
	}
	platform.ServeOps(ctx, platform.Env("OPS_ADDR", ":9101"), ready, log, nil)

	if url := platform.Env("MQTT_URL", ""); url != "" {
		// MQTT_CLIENT_ID should be stable across restarts (e.g. a StatefulSet
		// ordinal) before enabling MQTT_PERSISTENT_SESSION.
		host, _ := os.Hostname()
		clientID := platform.Env("MQTT_CLIENT_ID", "gw-"+host)
		persistent := strings.EqualFold(platform.Env("MQTT_PERSISTENT_SESSION", "false"), "true")
		tlsCfg, err := platform.ClientTLS("MQTT")
		if err != nil {
			log.Error("mqtt tls", "err", err)
			os.Exit(1)
		}
		_, err = ingest.StartMQTT(ctx, ingest.MQTTConfig{URL: url, ClientID: clientID, PersistentSession: persistent,
			Username: platform.Env("MQTT_USERNAME", ""), Password: platform.Env("MQTT_PASSWORD", ""),
			Topic: platform.Env("MQTT_TOPIC", "$share/ingest/fleetpulse/oem/+/telemetry"), TLS: tlsCfg}, g, log)
		if err != nil {
			log.Error("mqtt", "err", err)
			os.Exit(1)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", g.HTTPHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: platform.Env("HTTP_ADDR", ":8080"), Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = srv.Shutdown(sctx) // drain in-flight batches
		_ = cl.Flush(sctx)
	}()
	log.Info("ingest gateway listening", "addr", srv.Addr, "adapters", reg.List())
	cert, key := platform.Env("TLS_CERT_FILE", ""), platform.Env("TLS_KEY_FILE", "")
	if cert != "" {
		err = srv.ListenAndServeTLS(cert, key)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}
