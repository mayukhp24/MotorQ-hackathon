#!/bin/sh
# Hash credentials from the environment at start-up (never committed).
set -e
touch /mosquitto/data/passwd
chmod 0700 /mosquitto/data/passwd
mosquitto_passwd -b /mosquitto/data/passwd gateway "${MQTT_GATEWAY_PASSWORD}"
mosquitto_passwd -b /mosquitto/data/passwd oem-connector "${MQTT_CONNECTOR_PASSWORD}"
chown mosquitto:mosquitto /mosquitto/data/passwd
exec mosquitto -c /mosquitto/config/mosquitto.conf
