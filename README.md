# picohub

An HTTP server that manages locally-connected USB boards — Raspberry Pi Pico /
Pico2 today, ESP32c3/s3 next. It continuously captures each board's serial
output to a durable log and exposes a web UI to **reflash** a board and
**browse its logs**.

It is the long-running successor to the one-shot
[`picodeploy/deploy.sh`](../_import-examples/picodeploy/deploy.sh): instead of
build-and-flash-once, picohub watches every board on the bus, keeps a history,
and lets you reflash from the browser.

## Status

- **Phase 1 (done):** Raspberry Pi Pico / Pico2 — discovery, live serial
  logging, flashing via the BOOTSEL mass-storage flow, log browsing.
- **Phase 2 (planned):** ESP32c3/s3 flashing via
  [`tinygo.org/x/espflasher`](https://github.com/tinygo-org/espflash). Console
  monitoring already works; flashing currently returns "unsupported".

## Run

```sh
go run .                       # serve on :8080, db ./picohub.db, logs ./picohub-logs
go run . -addr :9000 -debug    # custom address, verbose logging
```

Flags: `-addr`, `-db`, `-logs`, `-poll` (discovery interval), `-ring` (per-device
console tail size, KiB), `-debug`. Open the address in a browser.

Mounting a Pico's BOOTSEL volume uses `udisksctl` (root-free) with a
`sudo -n mount` fallback — install `udisks2` or allow passwordless `mount`.

## How it works

- **Discovery** ([discover.go](discover.go)) reads sysfs for each USB tty's
  VID/PID and classifies it into a `Target`. IDs are stable across reboots (USB
  serial number, else `vid:pid@usb-path`).
- **Manager** ([manager.go](manager.go)) runs a per-board *console pump* that
  streams serial output to an on-disk log, an in-memory tail, and live SSE
  subscribers. It serializes flashing against the pump: stop pump → end session →
  flash → re-establish monitoring on the rebooted board.
- **Device** ([device.go](device.go)) is the per-board abstraction —
  `Read/Write` console, `EnterBootMode`, `Flash`, `MountDir`. `rp2Device`
  implements the 1200-baud BOOTSEL touch + mount + `.uf2` copy; `espDevice` is
  the Phase 2 stub.
- **Store** ([store.go](store.go)) keeps device/session/flash metadata in bbolt;
  raw serial bytes live as append-only per-session files under `-logs`.
- **Web** ([server.go](server.go), [html.templ](html.templ)) is `net/http` +
  [Templ](https://templ.guide) + HTMX + SSE: a dashboard, a per-device page
  (live console, flash/boot-mode/rename, history), and a log browser.

## Develop

```sh
go generate ./...   # regenerate html_templ.go after editing html.templ
go test -race ./...
```

This is a nested module (its own `go.mod`) so the tinyboot library stays
dependency-free; it imports the parent's `build/uf2` to validate uploads.
