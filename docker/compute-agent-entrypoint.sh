#!/bin/sh
# compute-agent needs a reachable iscsid (open-iscsi) to attach Volumes
# over iSCSI (internal/compute-agent/iscsi, docs/specs/volume.md) -- but,
# unlike everything else this entrypoint used to start itself, NOT one
# this script starts: a real iSCSI login's kernel session creation (a
# NETLINK_ISCSI socket) only works from the *host's* own network
# namespace, so iscsid has to be the host's already-running instance
# (this container's `pid: host` + internal/compute-agent/iscsi's
# `nsenter --net=/proc/1/ns/net` reach it there), not a fresh one started
# inside this container's own namespace -- see
# internal/compute-agent/iscsi's package doc comment for the full story,
# including why an earlier version of this script tried to start iscsid
# itself and that didn't work. A container-local iscsid would in fact
# actively conflict with the host's: open-iscsi's IPC socket is an
# abstract (network-namespace-scoped) one, so once nsenter puts this
# container in the host's namespace, a second iscsid bound there fails
# with "Can not bind IPC socket" instead of doing anything useful.
#
# playground/docker-compose.yml's compute-agent services depend on this
# dev host already running iscsid via its normal systemd unit -- the same
# kind of host prerequisite as /dev/kvm for Firecracker (see
# docs/specs/firecracker-boot.md).
exec /usr/local/bin/compute-agent "$@"
