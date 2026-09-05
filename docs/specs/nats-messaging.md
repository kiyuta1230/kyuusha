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

```go
type CreateCommand struct {
	VMID     string `json:"vm_id"`
	TenantID string `json:"tenant_id"`
	ImageID  string `json:"image_id"`
	VCPU     int32  `json:"vcpu"`
	MemoryMB int64  `json:"memory_mb"`
}

type CreateResult struct {
	VMID    string `json:"vm_id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type HeartbeatMsg struct {
	Hypervisor string    `json:"hypervisor"`
	At         time.Time `json:"at"`
}
```

## トレース伝播

NATSメッセージのヘッダへのW3C `traceparent`伝播と、consume側でのSpan Link化、`vm_id`相関の
詳細は[トレーシング仕様](observability-tracing.md)を参照。実装済み。
