#!/usr/bin/env bash
# playground/frr-type5-clos/cleanup.sh -- tears down the lab run-test.sh
# deploys (see its own header comment).
set -euo pipefail
cd "$(dirname "$0")"
sudo containerlab destroy -t topo.clab.yml --cleanup
