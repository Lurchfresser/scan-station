# scan-station

Weboberfläche für den Netzwerk-Scanner (Brother MFC-5440CN über SANE/brscan2).
Jede Person meldet sich mit einer PIN an, startet einen Scan und bekommt ein
PDF. Scans gehen entweder per `rclone` in die ocis-Cloud oder liegen zum
Download bereit.

## Bauen

```bash
go build -o scan-station .
```

Keine externen Go-Abhängigkeiten, nur die Standardbibliothek.

## Voraussetzungen auf dem Host

```bash
sudo apt install sane-utils img2pdf rclone
sudo apt install ocrmypdf tesseract-ocr-deu   # optional, macht PDFs durchsuchbar
```

Der Scanner muss bereits funktionieren:

```bash
scanimage -L        # muss brother2:net1;dev0 zeigen
```

## Einrichten

```bash
./scan-station -init            # legt config.json an
$EDITOR config.json             # Namen, PINs und Ziele anpassen
./scan-station
```

Die Adresse steht danach im Log, zum Beispiel `http://192.168.178.50:8080`.

### config.json

| Feld             | Bedeutung                                                   |
|------------------|-------------------------------------------------------------|
| `scanner`        | SANE-Gerät, hier `brother2:net1;dev0`                       |
| `listen`         | Adresse und Port, `:8080` lauscht auf allen Schnittstellen  |
| `data_dir`       | Ablage für Download-Scans, je Person ein Unterordner        |
| `retention_days` | Dateien älter als N Tage werden gelöscht, `0` schaltet aus  |
| `ocr_lang`       | Sprache für ocrmypdf, `deu` oder `deu+eng`                  |
| `paper_*_mm`     | Scanfläche, Standard A4. Legal wäre 216 × 356               |

Pro Person:

| Feld            | Bedeutung                                                      |
|-----------------|----------------------------------------------------------------|
| `id`            | interner Name, bestimmt den Ordner. Buchstaben, Ziffern, `-_`  |
| `name`          | Anzeigename in der Oberfläche                                  |
| `pin`           | mindestens 4 Zeichen                                           |
| `target`        | `ocis` für Upload, `download` für Ablage auf dem Server        |
| `rclone_remote` | nur bei `ocis`, zum Beispiel `ocis:Scans`                      |

### rclone für ocis

```bash
rclone config
```

Neues Remote, Typ `webdav`, `url` auf `https://<ocis>/remote.php/webdav`,
`vendor` auf `owncloud`. Als Passwort besser ein App-Token aus der
ocis-Weboberfläche verwenden als das Kontopasswort. Testen:

```bash
rclone lsd ocis:
rclone mkdir ocis:Scans
```

Läuft der Dienst später unter einem eigenen Systembenutzer, muss dessen
rclone-Konfiguration erreichbar sein, etwa über
`RCLONE_CONFIG=/var/lib/scan-station/rclone.conf`.

## Mit Docker

Das Image bringt `scanimage` und den Brother-Treiber brscan2 selbst mit, du
brauchst auf dem Host nichts zu installieren. **Nur x86_64**, denn brscan2
gibt es nur als amd64-Paket.

```bash
cp config.docker.json config.json
$EDITOR config.json                 # Namen und PINs setzen
$EDITOR docker-compose.yml          # SCANNER_IP, PUID/PGID (`id -u`, `id -g`)
docker compose up -d --build
docker compose logs -f
```

Danach läuft die Oberfläche auf `http://<host>:8080`.

Wichtig: `config.json` **vor** dem ersten Start anlegen. Fehlt die Datei,
erzeugt Docker an ihrer Stelle ein Verzeichnis, und der Container bricht mit
einer Fehlermeldung ab.

Im Log solltest du nach dem Start die Zeile mit `Devices on network` und darin
deine Scanner-IP sehen. Erscheint stattdessen die Warnung, dass `scanimage`
den Scanner nicht findet, stimmt `SCANNER_IP` nicht, oder das Gerät ist aus.

### Treiber ohne Internet

Der Build lädt brscan2 von Brother. Liegt das Paket schon vor, etwa aus dem
Installer auf dem Laptop, legst du es nach `driver/`, dann wird nichts
heruntergeladen:

```bash
cp ~/Downloads/brscan2-0.2.5-1.amd64.deb driver/
```

### Texterkennung

Standardmäßig ohne OCR, damit das Image klein bleibt. Für durchsuchbare PDFs
setzt du in `docker-compose.yml` `WITH_OCR: "true"` und baust neu. Wer die
Scans später an Paperless schickt, lässt es aus: Paperless erkennt den Text
selbst.

### Netzwerk

Der Container braucht nur ausgehende Verbindungen zum Scanner, deshalb genügt
das Bridge-Netz. Erst wenn die Scan-Taste am Gerät genutzt werden soll, muss
das Gerät umgekehrt den Dienst erreichen, und dann braucht es
`network_mode: host`.

## Wie es sich verhält

* Das Gerät bedient nur einen Auftrag auf einmal. Startet jemand einen Scan,
  sehen die anderen, wer gerade dran ist, und können nicht dazwischenfunken.
* Ein Scan dauert auf diesem Gerät mehrere Minuten pro Seite. Die Seite zeigt
  den Fortschritt live und lädt sich nach dem Ende selbst neu.
* Beim Stapelscan endet `scanimage` planmäßig mit einem Fehler, sobald der
  Einzug leer ist. Solange Seiten entstanden sind, gilt der Auftrag als
  erfolgreich.
* Scheitert der Upload in die Cloud, landet das PDF stattdessen in der
  Download-Ablage, damit der Scan nicht verloren geht.
* Jede Person sieht und lädt nur ihre eigenen Dateien. Dateinamen werden
  geprüft, Pfade lassen sich nicht ausbrechen.
* Nach sechs Fehlversuchen ist die Anmeldung von dieser IP zehn Minuten
  gesperrt.

## Sicherheitshinweise

Der Dienst spricht **HTTP, nicht HTTPS**, und ist für ein vertrauenswürdiges
Heimnetz gedacht. PINs und Cookies gehen unverschlüsselt über das WLAN. Wer
mehr braucht, setzt einen Reverse Proxy mit TLS davor. Ins Internet gehört der
Dienst nicht.

Die PINs stehen im Klartext in `config.json`. Datei auf `0600` setzen und dem
Dienstbenutzer zuweisen.

## Dateien

| Datei       | Inhalt                                               |
|-------------|------------------------------------------------------|
| `main.go`   | Start, Flags, Prüfung der benötigten Programme       |
| `config.go` | Konfiguration lesen und prüfen                       |
| `scan.go`   | Auftragsablauf: scannen, PDF, OCR, Upload, Ablage    |
| `server.go` | HTTP-Routen, Sitzungen, PIN-Prüfung                  |
| `views.go`  | HTML-Vorlagen und CSS                                |
| `util.go`   | Hilfsfunktionen, erlaubte Werte, Prüfmuster          |
