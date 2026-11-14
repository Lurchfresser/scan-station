#!/bin/sh
#
# Registriert den Netzwerkscanner beim Brother-Treiber und startet dann den
# Dienst als unprivilegierter Benutzer.
set -eu

: "${SCANNER_NAME:=MFC-5440CN}"
: "${SCANNER_MODEL:=MFC-5440CN}"
: "${CONFIG:=/etc/scan-station/config.json}"
: "${PUID:=1000}"
: "${PGID:=1000}"

if [ -z "${SCANNER_IP:-}" ]; then
    echo "FEHLER: SCANNER_IP ist nicht gesetzt (z.B. SCANNER_IP=192.168.178.98)." >&2
    exit 1
fi

if [ ! -f "$CONFIG" ]; then
    echo "FEHLER: Keine Konfiguration unter $CONFIG." >&2
    echo "        config.json als Volume einbinden, Vorlage: config.docker.json" >&2
    exit 1
fi

# Der Treiber merkt sich Geraete in einer eigenen Datei. Erst entfernen, dann
# neu anlegen, damit ein Neustart mit geaenderter IP sauber durchlaeuft.
brsaneconfig2 -r "$SCANNER_NAME" >/dev/null 2>&1 || true
brsaneconfig2 -a name="$SCANNER_NAME" model="$SCANNER_MODEL" ip="$SCANNER_IP"

# brsaneconfig2 -q listet zuerst alle unterstuetzten Modelle. Interessant ist
# nur der Teil ab "Devices on network".
echo "Registrierte Geraete:"
brsaneconfig2 -q 2>/dev/null | sed -n '/Devices on network/,$p' || true

chmod -R a+rX /opt/brother 2>/dev/null || true

# Erreichbarkeit einmal pruefen, damit ein Tippfehler in der IP sofort
# auffaellt und nicht erst beim ersten Scan.
if ! scanimage -L 2>/dev/null | grep -q "brother2"; then
    echo "WARNUNG: scanimage findet den Scanner nicht." >&2
    echo "         Pruefen: ist $SCANNER_IP erreichbar und das Geraet eingeschaltet?" >&2
    echo "         Der Dienst startet trotzdem, Scans werden aber fehlschlagen." >&2
fi

mkdir -p /data
chown -R "$PUID:$PGID" /data

echo "Starte scan-station als UID $PUID / GID $PGID"
exec setpriv --reuid "$PUID" --regid "$PGID" --clear-groups \
    /usr/local/bin/scan-station -config "$CONFIG"
