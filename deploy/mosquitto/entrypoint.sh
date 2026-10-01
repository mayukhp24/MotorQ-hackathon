#!/bin/sh
# Hash credentials from the environment at start-up (never committed).
# Config files are copied without carriage returns first: a Windows checkout
# can give them CRLF line endings, which mosquitto would read into topic names.
set -e
for f in mosquitto.conf acl; do tr -d '\r' < "/mosquitto/config/$f" > "/mosquitto/data/$f"; done
touch /mosquitto/data/passwd
chmod 0700 /mosquitto/data/passwd /mosquitto/data/acl
mosquitto_passwd -b /mosquitto/data/passwd gateway "${MQTT_GATEWAY_PASSWORD}"
mosquitto_passwd -b /mosquitto/data/passwd oem-connector "${MQTT_CONNECTOR_PASSWORD}"
chown mosquitto:mosquitto /mosquitto/data/passwd /mosquitto/data/acl
exec mosquitto -c /mosquitto/data/mosquitto.conf
