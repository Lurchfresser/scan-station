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
