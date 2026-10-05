// Command nftacl-snap is the built-in nftacl ACL backend (internal/
// compute-agent/nftacl, anti-spoofing and routed-traffic enforcement
// included) packaged as a standalone SNAP plugin: "nftacl-snap attach",
// "nftacl-snap detach" or "nftacl-snap update_sets" with the JSON payload
// on stdin, success is exit code 0 -- see docs/specs/snap.md. compute-agent behaves identically with
// -security-backend-bin pointing here or left empty; this exists so a SNAP
// shim can hand some interfaces to stock nftacl while handling others
// itself, and as a stand-alone implementation of the contract alongside
// examples/snap-plugins/ebpf-snap.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/snap"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: nftacl-snap attach|detach|update_sets  (JSON payload on stdin)")
		os.Exit(2)
	}
	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nftacl-snap: read stdin: %v\n", err)
		os.Exit(1)
	}
	var req snap.PluginRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		fmt.Fprintf(os.Stderr, "nftacl-snap: parse request: %v\n", err)
		os.Exit(1)
	}
	if err := snap.ServeBuiltin(os.Args[1], req); err != nil {
		fmt.Fprintf(os.Stderr, "nftacl-snap: %s %s: %v\n", os.Args[1], req.TapName, err)
		os.Exit(1)
	}
}
