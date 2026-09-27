# 仕様書

完成した機能単位の現状の仕様のみを記す。ここには過去の経緯や理由、日付は書かない——
「なぜこの設計か」は[docs/architecture.md](../architecture.md)、「いつ何が変わったか」は
[docs/release-notes.md](../release-notes.md)を参照。実装が変わったら都度この仕様書も追従して更新する。

## 一覧

- [システム構成](system-overview.md)
- [認証・認可](authn-authz.md)
- [Hypervisor登録・死活監視](hypervisor-bootstrap.md)
- [VMスケジュール](vm-scheduling.md)
- [Quota](quota.md)
- [Image](image.md)
- [VirtualMachine](virtual-machine.md)
- [Firecracker起動](firecracker-boot.md)
- [cloud-hypervisor起動](cloud-hypervisor-boot.md)
- [network](network.md)
- [VNAP（VM Network Attach Protocol）](vnap.md)
- [SNAP（Security Network Attach Protocol）](snap.md)
- [Volume](volume.md)
- [外部システム連携](external-integration.md)
- [CLI](cli.md)
- [NATSメッセージ](nats-messaging.md)
- [トレーシング](observability-tracing.md)
- [メトリクス](observability-metrics.md)
- [監査ログ](audit-logging.md)
