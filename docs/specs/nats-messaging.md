# NATSメッセージ仕様

コントロールプレーンサービスと、自身のハイパーバイザーagent群との間の非同期メッセージング規約。
サービス境界は跨がない（あるサービスは自分自身のagentとしか話さない。他サービスへは必ず相手の
gRPC APIを経由する）。以下のルールは現在実装済みのcomputeサービスを参照実装とし、
network/block-storage等、将来追加される他サービスのagent通信も同じ規約に従う。

## Subject命名規則

```
ms.<service>.<cmd|evt>.<hypervisor>.<resource-type>.<verb>
```

- コマンド（control-plane → agent）: 例 `ms.compute.cmd.hypervisor-1.vm.create`
- イベント（agent → control-plane）: 例 `ms.compute.evt.hypervisor-1.vm.create-result`,
  `ms.compute.evt.hypervisor-1.heartbeat`

## ストリームと配信保証

| ストリーム | 対象subject | Retention | 用途 |
|---|---|---|---|
| `<SERVICE>_CMD` | `ms.<service>.cmd.>` | WorkQueue（消費後に消える） | control-plane→agentの指示 |
| `<SERVICE>_EVT` | `ms.<service>.evt.>` | Limits（MaxAge 24時間） | agent→control-planeの結果報告・heartbeat |

- **CMD**: at-least-once。agentは処理の**完了**ではなく**受理**時点でackする。再配送されても
  安全に再適用できるよう、対象リソースは冪等な決定的IDを持つこと
- **EVT（結果報告）**: at-least-once。受信側（control-plane）は`resource_version`による
  楽観的並行性制御で冪等に反映する
- **EVT（heartbeat）**: ackなしのfire-and-forget。次回送信が数秒後に来るため1回の欠落は無害

## エンコーディング

JSON。protobufは使わない（gRPC APIとは異なる領域として意図的に単純化している）。
共通のenvelope型は設けず、各メッセージ型がそれぞれ必要なフィールドだけをフラットに持つ。

## メッセージ型の構造規約

同じ役割のメッセージ型は、サービスをまたいでも同じ形にする。

| 役割 | 必須フィールド | 例 |
|---|---|---|
| コマンド | 対象リソースの決定的ID（`<resource>_id`）、テナントスコープのリソースなら`tenant_id`、その他コマンド固有のフィールド | `CreateCommand{vm_id, tenant_id, image_id, vcpu, memory_mb}` |
| 結果報告イベント | 対象リソースの決定的ID、`success bool`、`error string`（失敗時のみ、`omitempty`） | `CreateResult{vm_id, success, error}` |
| heartbeatイベント | ハイパーバイザーID、送信時刻 | `HeartbeatMsg{hypervisor, at}` |

コマンド・結果報告イベントが必ず持つ対象リソースの決定的ID（`vm_id`等）は、後述する
トレース相関の主キーも兼ねる。

### 現在の実装（compute、`internal/compute/nats.go`）

実際のフィールドは機能追加のたびに増えている（イメージdigest検証、QCOW2、
NetworkInterface/UserData/Volume配線など）ので、下記は形を示す抜粋——全フィールドの
詳細な由来コメントは`internal/compute/nats.go`の`CreateCommand`自体を参照:

```go
type CreateCommand struct {
	VMID         string `json:"vm_id"`
	TenantID     string `json:"tenant_id"`
	ImageID      string `json:"image_id"`
	VCPU         int32  `json:"vcpu"`
	MemoryMB     int64  `json:"memory_mb"`
	DriverHint   string `json:"driver_hint"`
	KernelURL    string `json:"kernel_url,omitempty"`
	RootfsURL    string `json:"rootfs_url,omitempty"`
	KernelDigest string `json:"kernel_digest,omitempty"` // イメージdigest検証、docs/specs/image.md参照
	RootfsDigest string `json:"rootfs_digest,omitempty"`
	DiskURL      string `json:"disk_url,omitempty"` // QCOW2用、KernelURL/RootfsURLと排他
	DiskDigest   string `json:"disk_digest,omitempty"`
	BootArgs     string `json:"boot_args,omitempty"`
	Interfaces []NetworkInterfaceInfo `json:"interfaces,omitempty"` // tap配線用、network.md参照
	UserData   string                  `json:"user_data,omitempty"`  // cloud-init NoCloud seed disk
	Volumes    []VolumeAttachInfo      `json:"volumes,omitempty"`    // アタッチ済みVolume、volume.md参照
}

type CreateResult struct {
	VMID    string `json:"vm_id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// fire-and-forget: no result event (see docs/specs/firecracker-boot.md)
type DeleteCommand struct {
	VMID string `json:"vm_id"`
}

type HeartbeatMsg struct {
	Hypervisor string    `json:"hypervisor"`
	At         time.Time `json:"at"`
}
```

## コンソールアクセス: reply-subject方式（cmd/evtとは別系統）

`VirtualMachineService.StreamConsole`（[Firecracker起動仕様](firecracker-boot.md)参照）は
上記のcmd/evt/work-queueパターンに乗らない。理由は、(1) 配信保証・再配送が要らない
ライブ/エフェメラルなデータであること、(2)応答が複数メッセージにまたがる
ストリームであること、(3) `follow`時は呼び出し元がキャンセルするまで続く可能性があること。
このため`ms.compute.cmd.>`/`ms.compute.evt.>`のどちらにも属さない専用subject
（`ms.compute.console.<hypervisor>.request`）を使い、JetStreamのどちらのstreamにも
乗らない**プレーンNATS core**（永続化なし、at-most-once）で実装している。

```
ms.compute.console.<hypervisor>.request
```

1. Reconcilerが`nc.NewInbox()`で使い捨ての返信subject（`reply_subject`）を作り、
   `ConsoleRequest{vm_id, tail_bytes, follow, reply_subject}`を上記subjectへpublishする
2. compute-agentは`reply_subject`へ生バイト列のchunkを直接publishする（JSON envelopeなし）。
   最初の1通は空メッセージでも即座に送る（履歴が空でもReconciler側の初回応答待ちが
   タイムアウトしないように）
3. 終了は`reply_subject`宛のメッセージの`Kyuusha-Console-Done`ヘッダで示す
   （失敗時は`Kyuusha-Console-Error`ヘッダにメッセージを乗せる）
4. `follow`時、呼び出し元（gRPCクライアント）が切断すると、Reconcilerが
   `<reply_subject>.stop`へpublishしてcompute-agent側のtailループを止める
   （念のためcompute-agent側にも30分の安全上限がある）

## トレース伝播

NATSメッセージのヘッダへのW3C `traceparent`伝播と、consume側でのSpan Link化、`vm_id`相関の
詳細は[トレーシング仕様](observability-tracing.md)を参照。実装済み。
