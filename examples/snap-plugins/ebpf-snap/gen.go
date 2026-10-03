package main

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -type rule -type conntrack_key -type spoof_cfg bpf bpf/snap.c -- -I/usr/include
