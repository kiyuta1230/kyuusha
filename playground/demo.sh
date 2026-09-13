#!/usr/bin/env bash
# playground/demo.sh -- narrated product demo of kyuusha's playground stack.
#
# Unlike playground/scenario.sh (a strict CI-style regression test: hard
# assertions, immediate exit(1) on any deviation, exercises every edge case),
# this script is meant to be read aloud to an audience. It pauses between
# sections (Enter to continue) and narrates each step in Japanese as it
# walks through the product end to end: tenant -> image -> a real Firecracker
# VM boot -> network -> storage -> the VM resource metrics/Grafana
# dashboards added 2026-09-13. Pass -y to run non-interactively (no pauses --
# useful for smoke-testing this script itself, not for an actual demo).
#
# Brings the stack up itself if it isn't already running (same
# `docker compose up -d --build` scenario.sh uses) -- first run can take a
# while (real image builds); subsequent runs are fast (cached).
set -euo pipefail
cd "$(dirname "$0")/.."

auto=false
if [ "${1:-}" = "-y" ]; then auto=true; fi

pause() {
  [ "$auto" = true ] && return
  echo
  read -rp "  -- Enterで続行 -- " _
  echo
}

banner() {
  echo
  echo "================================================================"
  echo "  $1"
  echo "================================================================"
  echo
}

banner "kyuusha デモ -- 「OpenStackには大きすぎるが、VMwareライセンスは厳しい」規模向けの小さなIaaS/KaaS基盤"

echo "このデモは実際に動くkyuushaのplaygroundスタックに対して、CLIから本物の操作を行います。"
echo "モックは一切ありません -- 実Firecracker VM、実IPAM、実ブロックストレージ参照、実cgroupメトリクスです。"
pause

echo "==> playgroundスタックの起動確認"
if ! (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null; then
  echo "    api-gatewayが応答していません。スタックを起動します（初回はイメージビルドで数分かかります）"
  mkdir -p playground/volume-data
  chmod 0777 playground/volume-data
  # prometheus/grafanaを明示的に除外: このデモ自体はどちらにも依存しない
  # （Grafanaは案内するだけで、クリックスルーは前提にしていない）ので、
  # ホスト側でポート9090/3000を既に別プロセスが使っている環境（このホストの
  # 開発環境がまさにそう）でもデモ全体を巻き込んで失敗させたくない。
  # フルスタックを試したい場合は `docker compose -f playground/docker-compose.yml
  # up -d --build` を直接使うか、playground/scenario.sh を参照。
  docker compose -f playground/docker-compose.yml up -d --build \
    etcd nats jaeger loki promtail image-assets identity image \
    network network-reconciler block-storage block-storage-reconciler \
    compute compute-reconciler compute-agent-1 compute-agent-2 compute-agent-3 \
    api-gateway
  echo "==> api-gatewayの起動待ち"
  for _ in $(seq 1 60); do
    if (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null; then exec 3>&-; break; fi
    sleep 1
  done
else
  exec 3>&-
  echo "    起動済みのスタックを利用します"
fi

admin_token="$(go run ./cmd/kyuusha token mint -tenant=bootstrap-admin -role=admin -sub=demo-admin@example.com)"

banner "1. Hypervisorフリート -- compute-agentが自己登録したハイパーバイザー群"
echo "各compute-agentは起動時にbootstrapトークンでzoneスコープ付き自己登録します（PKIの類は使いません）。"
KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha hypervisor list -addr=localhost:8080
pause

banner "2. Tenant -- Quotaを持つマルチテナントの単位"
tenant_name="demo-$(date +%s)"
tenant_line="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant create -addr=localhost:8080 \
  -name="$tenant_name" -display-name="Demo Tenant" \
  -max-vcpu=4 -max-memory-mb=4096 -max-volume-gb=10 -max-vms=4 -max-vcpu-per-vm=2 -max-memory-mb-per-vm=2048)"
echo "$tenant_line"
tenant="$(echo "$tenant_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
export KYUUSHA_TOKEN
KYUUSHA_TOKEN="$(go run ./cmd/kyuusha token mint -tenant="$tenant")"
echo "    -> tenant=$tenant のトークンを発行しました（以降はこのテナントとして操作します）"
pause

banner "3. Image -- kernel + rootfsの参照（kyuushaはblobを保管しません）"
image_line="$(go run ./cmd/kyuusha image create -addr=localhost:8080 -tenant="$tenant" -name=demo-image \
  -format=kernel_rootfs -kernel-url=http://image-assets/vmlinux -rootfs-url=http://image-assets/rootfs.ext4)"
echo "$image_line"
image="$(echo "$image_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
echo "==> Ready待ち"
for _ in $(seq 1 30); do
  phase="$(go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant" -id="$image" | grep -o 'phase=[^ ]*' | cut -d= -f2)"
  [ "$phase" = "Ready" ] && break
  sleep 1
done
echo "    -> phase=$phase"
pause

banner "4. VirtualMachine -- 実Firecracker microVMの起動"
echo "スケジューラが実際にハイパーバイザーを選び、compute-agentが本物のFirecrackerプロセスを起動します。"
vm_line="$(go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=demo-vm \
  -image="$image" -vcpu=1 -memory-mb=128 -wait)"
echo "$vm_line"
vm_id="$(echo "$vm_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
hypervisor="$(echo "$vm_line" | grep -o 'hypervisor=[^ ]*' | tail -1 | cut -d= -f2)"
echo
echo "==> シリアルコンソール（実ゲストが本当に起動したことの証拠）"
go run ./cmd/kyuusha vm console -addr=localhost:8080 -tenant="$tenant" -id="$vm_id" 2>/dev/null | tail -5 || true
pause

banner "5. Network -- Subnet + NetworkInterface（実IPAM、reconcilerによる即時割り当て）"
subnet_line="$(go run ./cmd/kyuusha subnet create -addr=localhost:8080 -tenant="$tenant" -name=demo-subnet -zone=zone-a -cidr=10.0.50.0/24 -gateway-ip=10.0.50.1)"
echo "$subnet_line"
subnet="$(echo "$subnet_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
netif_line="$(go run ./cmd/kyuusha netif create -addr=localhost:8080 -tenant="$tenant" -name=demo-netif -vm="$vm_id" -subnet="$subnet")"
echo "$netif_line"
echo "    -> Subnet/NetworkInterfaceともCreate直後にPending、次のWatchイベントで即座にReadyへ（10秒周期の"
echo "       フォールバックスイープを待つ必要はありません -- 2026-09-13のreconciler分離で入れた設計）"
pause

banner "6. Storage -- StorageConnection + Volume + VolumeAttachment"
mkdir -p playground/volume-data
if ! KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha storageconn list -addr=localhost:8080 2>/dev/null | grep -q 'name=playground-nfs'; then
  KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha storageconn create -addr=localhost:8080 -name=playground-nfs -zones=zone-a >/dev/null
fi
truncate -s 1M "playground/volume-data/demo-volume.img"
chmod 0666 "playground/volume-data/demo-volume.img"
volume_line="$(go run ./cmd/kyuusha volume create -addr=localhost:8080 -tenant="$tenant" -name=demo-volume -size-gb=1 -protocol=NFS -storage-connection=playground-nfs -identifier=demo-volume.img)"
echo "$volume_line"
volume="$(echo "$volume_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
echo "==> Ready待ち（StorageConnection到達確認 + identifier実在確認、両方非同期）"
for _ in $(seq 1 20); do
  phase="$(go run ./cmd/kyuusha volume get -addr=localhost:8080 -tenant="$tenant" -id="$volume" | grep -o 'phase=[^ ]*' | cut -d= -f2)"
  [ "$phase" = "Ready" ] && break
  sleep 1
done
echo "    -> phase=$phase"
attach_line="$(go run ./cmd/kyuusha volattach create -addr=localhost:8080 -tenant="$tenant" -name=demo-attach -vm="$vm_id" -volume="$volume")"
echo "$attach_line"
pause

banner "7. Observability -- 本日実装したVMリソースメトリクス（/metrics/resources）"
echo "compute-agentは自身の/metricsとは別に、cgroup由来の実VM CPU/メモリを/metrics/resourcesで公開します。"
echo "（コンテナに直接ポートは公開していないので、同じdockerネットワーク上から取得します）"
# hypervisorはkyuusha側の論理Hypervisor ID（例: hypervisor-2）で、dockerネットワーク上の
# コンテナ名（compute-agent-2）とは別物 -- playgroundの命名規約上、末尾の番号だけが一致する。
compute_agent_host="compute-agent-${hypervisor#hypervisor-}"
docker run --rm --network kyuusha-playground_default curlimages/curl:latest -s "http://${compute_agent_host}:9094/metrics/resources" | grep -v '^#' || true
echo
echo "Grafanaでは以下のダッシュボードが自動プロビジョニング済みです（http://localhost:3000）:"
echo "  - kyuusha overview（全サービス横断サマリ）"
echo "  - kyuusha: compute-agent（上記メトリクスのグラフ、VM CPU/メモリ推移）"
echo "  - kyuusha: compute / network / block-storage（それぞれAPI+reconcilerペア）"
echo "  - kyuusha: api-gateway / identity / image"
echo "Jaeger（http://localhost:16686）ではこのデモの一連のリクエストのトレースも辿れます。"
pause

banner "8. 後片付け"
echo "このデモ用のリソースだけ削除します（スタック自体は動かしたままにします）。"
go run ./cmd/kyuusha volattach delete -addr=localhost:8080 -tenant="$tenant" -id="$(echo "$attach_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)" || true
go run ./cmd/kyuusha volume delete -addr=localhost:8080 -tenant="$tenant" -id="$volume" || true
go run ./cmd/kyuusha netif delete -addr=localhost:8080 -tenant="$tenant" -id="$(echo "$netif_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)" || true
go run ./cmd/kyuusha subnet delete -addr=localhost:8080 -tenant="$tenant" -id="$subnet" || true
go run ./cmd/kyuusha vm delete -addr=localhost:8080 -tenant="$tenant" -id="$vm_id" || true
go run ./cmd/kyuusha image delete -addr=localhost:8080 -tenant="$tenant" -id="$image" || true
KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant delete -addr=localhost:8080 -id="$tenant" || true
rm -f "playground/volume-data/demo-volume.img"

echo
echo "デモ終了。スタックは動かしたままです -- 'docker compose -f playground/docker-compose.yml down' で後片付けできます。"
