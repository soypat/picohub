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

Mounting a Pico's BOOTSEL volume uses `udisksctl` (when a desktop session is
present) with a `mount` fallback. Both need privilege, so on a headless box run
picohub as **root** (see below).

## Flashing

Upload a **`.uf2`** and it is written to the board unchanged. Upload an
**`.elf`** and picohub converts it first, the same way `tinygo build -o x.uf2`
and [`picobin uf2conv`](https://github.com/soypat/tinyboot/tree/main/cmd/picobin)
do: take the contiguous flash image the ELF describes and wrap it in UF2 blocks
tagged with the chip's family id. The conversion is checked against TinyGo's own
output byte-for-byte in `flash/elf_test.go`.

What the upload *is* decides how it is treated, not what it is called — the ELF
magic is what picohub looks at.

The family id is taken from the board's family (`pico` -> `0xe48bff56`,
`pico2` -> `0xe48bff59`), so if that is wrong the bootrom will refuse the image
without saying why. Pin it on the debug page when discovery guesses wrong.

Keep the ELF if you also plan to debug: a `.uf2` carries no symbols, and gdb
needs the ELF (see below).

## Sharing a board with openocd (ignore)

A board can be marked **ignored** from its device page ("Release board"). picohub
then never opens its console, starts no log session and offers no flashing — it
only keeps listing the board and tracking present/absent — so `openocd`, `gdb` or
a terminal program can own the port. Releasing takes effect immediately: the
pump stops and the live session ends before the request returns. "Resume picohub
control" re-attaches on the next discovery poll.

The flag is per device and persisted, so it survives replug and restarts. Mark a
debug probe ignored once and picohub will leave it alone from then on — a probe
enumerates as an ordinary RP2 board, so picohub cannot tell on its own that the
port belongs to openocd.

## Remote debugging (openocd)

Each device has a **debug page** (`/devices/<id>/debug`, linked from the device
page) that turns the machine picohub runs on into a debug server: picohub
supervises an `openocd` process for the probe and serves the GDB remote protocol
on a TCP port. You run gdb on your own machine.

The ELF never leaves your machine for this. gdb reads DWARF from a local copy
and `load` writes flash through the remote protocol, so nothing but the remote
protocol crosses the network — the host only needs `openocd` installed. (This is
separate from uploading an `.elf` to *flash*, above, which does send the file.)

```sh
# on your machine, once a session is running
tinygo build -o out.elf -target=pico2 ./yourpkg
gdb-multiarch out.elf \
  -ex "target extended-remote picohub-host:3333" \
  -ex "monitor halt" -ex "load" -ex "monitor reset halt"
```

The debug page shows the probe's identity, the openocd config in use (interface
and target `.cfg` names plus adapter speed, persisted per device and defaulted
from the discovered chip), live openocd output, and the exact gdb command with
the right host and port filled in.

A session claims the board for as long as it runs: picohub closes the console
and will not re-attach until you stop the session. Discovery keeps tracking the
board as present or absent throughout.

> **The gdb port is unauthenticated.** Anyone who can reach it can halt the chip,
> read and write all of its memory, and reprogram the board. It defaults to
> listening on every interface (`-ocd-bind 0.0.0.0`). Pass
> `-ocd-bind 127.0.0.1` to keep sessions on the host and reach them through an
> SSH tunnel (`ssh -L 3333:localhost:3333 picohub-host`) instead. openocd's
> telnet and Tcl consoles, which expose far more than the gdb port does, are
> always disabled.

Flags: `-ocd-bind` (listen address, default `0.0.0.0`), `-ocd-port` (first port
a session takes, default `3333`; concurrent sessions take the next free ones),
`-openocd` (path to the binary), `-ocd-scripts` (openocd's config search path).

**Finding openocd.** picohub looks the binary up on *its own* `PATH`, which for a
systemd service is a minimal one — not your login shell's. An openocd built
outside the system prefixes will not be found even though typing `openocd`
works for you. Point picohub at it:

```
ExecStart=/usr/local/go/bin/go run /home/pato/picohub -openocd /home/pato/local/openocd/openocd
```

A relocated build that keeps its configs beside the binary (a `scripts/` or
`tcl/` directory next to it) is detected automatically; anything else needs
`-ocd-scripts`. The debug page shows which binary and search path were resolved.

**Permissions.** openocd talks to the probe's raw USB interface, which needs
either root or a udev rule. picohub already runs as root under systemd; run as
an ordinary user it fails with `unable to find a matching CMSIS-DAP device`
even though the probe is plugged in.

**The target is not the probe.** The `target/<name>.cfg` setting names the chip
*wired to* the probe over SWD, which picohub has no way to discover — it only
sees the probe's own USB id. The default is derived from that id, so it is only
correct when the board being debugged is the board picohub is listing. Set it
per device on the debug page.

**Board family.** Discovery infers the family from VID/PID, which cannot always
separate two chips: an RP2350-based Debug Probe enumerates as `2e8a:000c`
exactly like the RP2040-based one, so both are classified `pico`. The debug page
has a **Board family** selector that pins it (persisted per device, `auto`
clears the pin). Pinning it also changes the openocd target the debug config
defaults to.

## Run as a systemd service

Flashing mounts the BOOTSEL mass-storage volume and opens serial ports — both
privileged. The simplest reliable setup is to run the service as **root**;
there's no unix group that grants `mount(2)`, so a non-root user would need a
polkit rule or an `/etc/fstab` entry, which isn't worth the complexity here.

[`picohub.service`](picohub.service) runs as root (no `User=`). Install it with
[`deploy.sh`](deploy.sh), which syncs the tree, symlinks the unit into
`/etc/systemd/system/`, reloads, and restarts.

## How it works

- **Discovery** ([flash/discover.go](flash/discover.go)) reads sysfs for each USB
  tty's VID/PID and classifies it into a `Target`. IDs are stable across reboots
  (USB serial number, else `vid:pid@usb-path`).
- **Manager** ([manager.go](manager.go)) runs a per-board *console pump* that
  streams serial output to an on-disk log, an in-memory tail, and live SSE
  subscribers. It serializes flashing against the pump: stop pump → end session →
  flash → re-establish monitoring on the rebooted board.
- **Device** (the [flash](flash/) package) is the per-board abstraction —
  `Read/Write` console, `EnterBootMode`, `Flash`, `MountDir`. `rp2Device`
  implements the 1200-baud BOOTSEL touch + mount + `.uf2` copy; `espDevice` is
  the Phase 2 stub. The package is server-independent so it can be driven from
  the [cmd/picoflash](cmd/picoflash/) CLI or tests.
- **Store** ([store.go](store.go)) keeps device/session/flash metadata in bbolt;
  raw serial bytes live as append-only per-session files under `-logs`.
- **Firmware** ([flash/elf.go](flash/elf.go)) turns an uploaded `.elf` into a
  UF2 for the board's chip, using `tinyboot/build/elfutil` to extract the flash
  image and `tinyboot/build/uf2` to format it. `PrepareFirmware` is the one
  entry point both the server and the CLI use.
- **OCD** (the [ocd](ocd/) package) supervises an `openocd` process per probe:
  it builds and *validates* the command line (config names reach it from a web
  form, so they are rejected rather than escaped), waits for openocd's own
  "Listening on port N for gdb connections" before reporting success, and owns
  the process until it is stopped. Like `flash`, it is server-independent.
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
