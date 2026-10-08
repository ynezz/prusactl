package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// firmwareCmd is the terminal side of get_firmware_status and update_firmware.
func firmwareCmd(ctx context.Context, o options, pos []string) error {
	act, rest, err := action("firmware", pos, "status", "update")
	if err != nil {
		return err
	}
	if err := atMost("firmware "+act, rest, 0, "prusactl firmware "+act); err != nil {
		return err
	}
	args := printerArgs(o)
	if act == "status" {
		if err := notForAction(o, "firmware", act, "version"); err != nil {
			return err
		}
		return runTool(ctx, "get_firmware_status", args, o.on("json"), func(raw json.RawMessage) error {
			renderFirmwareStatus(os.Stdout, decodeMap(raw))
			return nil
		})
	}
	if v := o.str("version"); v != "" {
		args["version"] = v
	}
	return runTool(ctx, "update_firmware", args, o.on("json"), func(raw json.RawMessage) error {
		m := decodeMap(raw)
		fmt.Println(field(m, "note"))
		return nil
	})
}

// renderFirmwareStatus prints a get_firmware_status result.
func renderFirmwareStatus(w io.Writer, m map[string]any) {
	fmt.Fprintf(w, "%-9s %s\n", "Printer:", field(m, "printer"))
	fmt.Fprintf(w, "%-9s %s\n", "Running:", field(m, "current"))
	fmt.Fprintf(w, "%-9s %s  (%s)\n", "Latest:", field(m, "latest"), field(m, "state"))
	switch {
	case field(m, "latest") == "":
		fmt.Fprintln(w, "\nPrusa Connect doesn't say which firmware is the latest for this printer.")
	case field(m, "update_available") != "true":
		fmt.Fprintln(w, "\nUp to date.")
	case field(m, "update_supported") == "true":
		fmt.Fprintln(w, "\nAn update is available: `prusactl firmware update` installs it.")
	default:
		fmt.Fprintln(w, "\nAn update is available, but Prusa Connect doesn't offer it for this printer.")
	}
}
