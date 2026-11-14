# syntax=docker/dockerfile:1
#
# Nur fuer x86_64: Brothers brscan2-Treiber gibt es ausschliesslich als
# amd64-Binaerpaket. Auf ARM laeuft das Image nicht.

# ---------------------------------------------------------------- Bauen
FROM golang:1.23-bookworm AS build

WORKDIR /src
COPY go.mod ./
COPY *.go ./

# Statisches Binary, damit es im schlanken Laufzeit-Image ohne
# Go-Abhaengigkeiten startet.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/scan-station .

# ---------------------------------------------------------------- Laufzeit
FROM debian:bookworm-slim

# Mit WITH_OCR=true kommen ocrmypdf und Tesseract mit ins Image. Das macht es
# rund 600 MB groesser. Wer die PDFs spaeter an Paperless schickt, braucht es
# nicht: Paperless erkennt den Text selbst.
ARG WITH_OCR=false

# Falls Brother die Datei verschiebt, hier die neue Adresse eintragen. Liegt
# das Paket unter driver/ im Build-Kontext, wird es von dort genommen und
# nichts heruntergeladen.
ARG BRSCAN2_URL=https://download.brother.com/pub/com/linux/linux/packages/brscan2-0.2.5-1.amd64.deb

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        img2pdf \
        libusb-0.1-4 \
        netbase \
        libusb-1.0-0 \
        sane-utils \
        tzdata \
        util-linux \
    && if [ "$WITH_OCR" = "true" ]; then \
         apt-get install -y --no-install-recommends \
           ocrmypdf tesseract-ocr tesseract-ocr-deu tesseract-ocr-eng; \
       fi \
    && rm -rf /var/lib/apt/lists/*

# libsane-brother2 ist gegen das alte libusb-0.1 gelinkt (libusb-0.1-4),
# ohne das laedt SANE das Backend nicht und findet keinen Scanner.
# netbase liefert /etc/services; ohne die Datei stuerzt der Treiber beim
# Oeffnen des Netzwerkgeraets ab (getservbyname liefert NULL, Segfault).
# Brother-Scannertreiber. dpkg meldet bei diesen alten Paketen gern fehlende
# Abhaengigkeiten, die anschliessende Korrektur raeumt das auf.
COPY driver/ /tmp/driver/
RUN set -eu; \
    if ls /tmp/driver/brscan2*.deb >/dev/null 2>&1; then \
      echo "Treiber aus driver/ verwenden"; \
      cp /tmp/driver/brscan2*.deb /tmp/brscan2.deb; \
    else \
      echo "Treiber von Brother laden"; \
      curl -fsSL -o /tmp/brscan2.deb "$BRSCAN2_URL"; \
    fi; \
    apt-get update; \
    dpkg -i /tmp/brscan2.deb || apt-get install -y --no-install-recommends -f; \
    rm -rf /tmp/brscan2.deb /tmp/driver /var/lib/apt/lists/*; \
    test -x /usr/bin/brsaneconfig2 || test -x /usr/local/bin/brsaneconfig2

# Der Dienst liest die Netzwerkkonfiguration des Treibers, laeuft aber nicht
# als root. Deshalb muessen diese Dateien lesbar sein.
RUN chmod -R a+rX /opt/brother 2>/dev/null || true

# Das Paket legt seine Hilfsbibliotheken nach /usr/lib64, das Backend laedt
# sie aber per dlopen() aus den Standardpfaden. Ohne diese Links scheitert
# das Oeffnen des Geraets mit "Invalid argument".
RUN for f in /usr/lib64/libbr*.so*; do \
      ln -sf "$f" /usr/lib/x86_64-linux-gnu/; \
    done && ldconfig

# Nur das Brother-Backend laden. Sonst probiert SANE alle Backends durch,
# im Hostnetz u.a. Canons bjnp auf jeder Schnittstelle, was scanimage -L
# langsam macht und das Log mit Fehlern fuellt.
RUN echo brother2 > /etc/sane.d/dll.conf && rm -f /etc/sane.d/dll.d/*

COPY --from=build /out/scan-station /usr/local/bin/scan-station
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

VOLUME /data
EXPOSE 8080

ENV SCAN_LISTEN=:8080 \
    SCAN_DATA_DIR=/data \
    SCANNER_NAME=MFC-5440CN \
    SCANNER_MODEL=MFC-5440CN \
    CONFIG=/etc/scan-station/config.json \
    PUID=1000 \
    PGID=1000

# Port aus SCAN_LISTEN (z.B. ":8080"), sonst 8080.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD port="${SCAN_LISTEN##*:}"; curl -fsS "http://localhost:${port:-8080}/" -o /dev/null

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
