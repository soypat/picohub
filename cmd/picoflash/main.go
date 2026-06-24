// Command picoflash is a thin CLI over the flash package, for driving board
// discovery and flashing directly from the terminal — without the picohub HTTP
// server. It exists mainly to reproduce and debug flashing in isolation.
//
// Usage:
//
//	picoflash list
//	picoflash flash [-id ID] <firmware.uf2>
//	picoflash bootmode [-id ID]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/soypat/picohub/flash"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "list", "ls":
		err = cmdList()
	case "flash":
		err = cmdFlash(ctx, args)
	case "bootmode", "boot":
		err = cmdBootmode(ctx, args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "picoflash:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  picoflash list
  picoflash flash [-id ID] <firmware.uf2>
  picoflash bootmode [-id ID]
`)
	os.Exit(2)
}

func cmdList() error {
	devs, err := flash.NewDiscoverer().Scan()
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no boards found")
		return nil
	}
	for _, d := range devs {
		mode := "console"
		if d.BootSel {
			mode = "BOOTSEL"
		}
		port := d.Port
		if port == "" {
			port = "-"
		}
		fmt.Printf("%-24s  %-8s  %-9s  %s  %s\n", d.ID, d.Target, mode, d.VIDPID(), port)
	}
	return nil
}

func cmdFlash(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("flash", flag.ExitOnError)
	id := fs.String("id", "", "device ID to flash (defaults to the only RP2 board present)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("flash needs exactly one firmware path; got %d args", fs.NArg())
	}
	fwPath := fs.Arg(0)

	if err := flash.ValidateUF2(fwPath); err != nil {
		return fmt.Errorf("validate %s: %w", fwPath, err)
	}
	fi, err := os.Stat(fwPath)
	if err != nil {
		return err
	}

	desc, err := pickDevice(*id)
	if err != nil {
		return err
	}
	fmt.Printf("flashing %s (%d bytes) -> %s [%s] on %s\n", fwPath, fi.Size(), desc.ID, desc.Target, desc.Port)

	dev := flash.NewDevice(desc)
	defer dev.Close()

	f, err := os.Open(fwPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var lastPct = -1
	progress := func(done, total int64) {
		pct := -1
		if total > 0 {
			pct = int(done * 100 / total)
		}
		if pct != lastPct {
			lastPct = pct
			fmt.Printf("\r  copying: %d/%d bytes (%d%%)   ", done, total, pct)
		}
	}

	start := time.Now()
	err = dev.Flash(ctx, f, fi.Size(), progress)
	fmt.Println()
	if err != nil {
		return fmt.Errorf("flash failed after %s: %w", time.Since(start).Round(time.Millisecond), err)
	}
	fmt.Printf("flash OK in %s; board rebooting\n", time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdBootmode(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootmode", flag.ExitOnError)
	id := fs.String("id", "", "device ID (defaults to the only RP2 board present)")
	_ = fs.Parse(args)

	desc, err := pickDevice(*id)
	if err != nil {
		return err
	}
	dev := flash.NewDevice(desc)
	defer dev.Close()
	fmt.Printf("resetting %s [%s] into boot mode...\n", desc.ID, desc.Target)
	if err := dev.EnterBootMode(ctx); err != nil {
		return err
	}
	fmt.Println("done")
	return nil
}

// pickDevice resolves a Descriptor by ID, or returns the sole RP2 board when no
// ID is given.
func pickDevice(id string) (flash.Descriptor, error) {
	devs, err := flash.NewDiscoverer().Scan()
	if err != nil {
		return flash.Descriptor{}, err
	}
	if id != "" {
		for _, d := range devs {
			if d.ID == id {
				return d, nil
			}
		}
		return flash.Descriptor{}, fmt.Errorf("no board with id %q (run `picoflash list`)", id)
	}
	var rp2 []flash.Descriptor
	for _, d := range devs {
		if d.Target.IsRP2() {
			rp2 = append(rp2, d)
		}
	}
	switch len(rp2) {
	case 0:
		return flash.Descriptor{}, fmt.Errorf("no RP2 board found (run `picoflash list`)")
	case 1:
		return rp2[0], nil
	default:
		return flash.Descriptor{}, fmt.Errorf("%d RP2 boards present; pass -id to choose one", len(rp2))
	}
}
