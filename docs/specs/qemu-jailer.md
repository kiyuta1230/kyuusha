# QEMU用jailer設計（2026-09-12決定）

## 概要

`driver_hint=QEMU`（[QEMU起動仕様](qemu-boot.md)）で起動するVMは、現状
`internal/compute-agent/qemuvmm`が`qemu-system-x86_64`を直接execしているだけで、
chrootもuid/gid権限降格もseccompも無い。compute-agentコンテナ自体が特権コンテナ
（`/dev/kvm`、`NET_ADMIN`、privileged）なので、QEMUプロセス自身もroot・無隔離の
まま動いている。

`driver_hint=FIRECRACKER`側は2026-09に実jailer（AWS製、chroot + uid/gid降格）で
ラップ済み（[Firecracker起動仕様](firecracker-boot.md)「jailer」）。本ドキュメントは
QEMU側に同等以上の隔離を持たせるための設計であり、2026-09-11時点で一度「別プロジェクト
として汎用jailerを新規に立ち上げる」方針を決めていたものを2026-09-12に撤回し、
「既製の汎用exec型jailerをkyuusha本体に直接統合する」方針に変更した経緯を含む
（旧方針の記録は[QEMU jailerハンドオフ](../qemu-jailer-handoff.md)）。

## なぜjailerが必要か

- QEMUはC実装で、USB/NIC/ディスプレイアダプタ等の巨大なデバイスエミュレーション面を
  持つ。ゲスト起動からのエスケープが成功すると、そのままホストroot奪取に直結する
  リスクがある。
- Firecrackerとの対比: FirecrackerはRust製でvirtio-blk/virtio-net/serialのみの
  最小デバイスモデル。jailerは「既に小さい信頼基盤への多層防御」という位置付けだったが、
  QEMUはデバイスモデルが桁違いに大きく、外部隔離の価値がより高い。
- compute-agentコンテナ自体がtap配線等のため特権コンテナである以上、QEMUプロセス
  自身の権限を落とす・閉じ込める作業は必ず外側から行う必要がある（compute-agent
  自体の特権は削れない）。
- 副次的な発見: QEMU自体が`-sandbox`オプションで内蔵seccompを持つ（Firecracker本体が
  内蔵seccompを持つのと同じ構図）。fork/exec禁止・特権昇格禁止等はQEMU自身が
  カバーできるため、外部jailer側のseccompは「防御の多層化」という位置付けになる。
  QEMU公式ドキュメント（qemu.org/docs/master/system/security.html）もsandboxingの
  必要性に言及している。

## 方針転換の理由（2026-09-11版との差分）

2026-09-11時点のハンドオフ文書は、別プロジェクトを切り出す理由として以下2点を
挙げていた:

1. QEMU向けにはFirecracker用jailerのような既製ツールが無い
2. namespace分離+seccompという一番リスクの高い部分でkyuushaのロードマップを
   ブロックしたくない

2026-09-12の調査でこの2つの前提が両方崩れた:

- **前提1は誤り**: nsjail（Google）、minijail（Google/ChromeOS）という、
  chroot + namespace（PID/mount/net）+ seccomp-bpf + uid/gid降格を一通り持つ
  既製の汎用exec型jailerが存在する。
- **前提2も弱まる**: 自前でnamespace/seccompエンジンを実装するのではなく既製
  バイナリを呼ぶだけなら、リスクはFirecracker用jailerを呼んでいる現状の実装と
  ほぼ同水準まで下がる。

## 既存OSS調査結果

| ツール | chroot | namespace | seccomp | 特記事項 |
|---|---|---|---|---|
| **minijail（推奨）** | ○ | PID/mount/user/net | 独自ポリシー構文 | crosvm（ChromeOS/AndroidのRust製VMM）がデバイスプロセスのjailingに実際に使用——VMM jailingという今回と同じ用途の実例。2026年も活発にメンテ（直近リリース2026.05.18） |
| nsjail（代替） | ○ | PID/mount/net/uts/ipc/cgroup | Kafelポリシー言語 | より一般的な用途（オンラインジャッジ/CTF基盤等）向けだが同等機能を持つ |
| bubblewrap | ○ | mount/user中心 | 外部BPFプログラムを渡す形（自前コンパイラ無し） | rootless設計、Flatpak基盤 |
| runc | ○（pivot_root） | 全種 | ○（OCI JSON） | 最も枯れているがOCIバンドル（rootfsツリー）前提でやや重い |

**minijailを第一候補として推奨する**。crosvmでのVMM jailing実績が、他候補には
ない直接的な前例のため。最終選定は実機QEMUに対する動作検証で確定させる、未決事項
として残す。

## v1スコープ

- **含む**: chroot + PID/mount namespace分離 + seccomp + uid/gid降格
  （minijail/nsjailにまるごと任せる）
- **含まない（v1）**: network namespace分離。`fcvmm/manager.go`の既存コメント通り、
  Firecracker側も現状「`--netns`を使わずtapはcompute-agentコンテナ自身のnetnsに
  置いたまま」というスコープなので、QEMU側もこれに揃える。tapを新netnsへ移動する
  際の「minijail/nsjailが新netnsを作った直後、execする前に外部からinterfaceを
  移動する」同期問題は、実装が複雑になる割に今すぐ必要な要件ではないため先送りする。

## kyuusha側の実装イメージ

- `internal/compute-agent/qemuvmm/jail_stage.go`（新規）: `fcvmm/jailer.go`の
  5関数（`resolveExecPath`、`jailChrootDir`、`placeReadOnlyResource`、
  `placeWritableResource`、`mknodDeviceLike`、`placeVolumeLike`）をQEMU側にも
  流用・一般化して複製する（Firecracker固有の命名を外すのみ、大きな設計変更は
  不要——実装コストは低い）。
- `qemuvmm/manager.go`の`Boot()`内、現在`exec.Command(m.binPath(), args...)`で
  QEMUを直接execしている箇所を、`exec.Command("minijail0", jailArgs...)`経由に
  変更する（`fcvmm/manager.go`がjailerバイナリを呼んでいる形と対称的）。
- 具体的なminijail0フラグ例: `-u <uid> -g <gid> -C <chroot> --seccomp_policy_file
  <policy> -- <qemu-exec-path> <qemu-args...>` のような形（実装フェーズで
  minijailのmanページを見ながら確定）。

## 未決事項

- minijail vs nsjailの最終選定（実機QEMUを実際にjailして起動できるか検証してから）
- QEMU向けseccompポリシーの具体的な内容（strace等でのトレースに基づく作成が
  別途必要、当て推量で埋めない——旧ハンドオフ文書が指摘していたのと同じ理由）。
  **2026-09-12調査**: minijail/nsjail側に実QEMU（`qemu-system-*`）向けの既製
  ポリシーは無い（crosvm自身のseccompポリシー、`jail/seccomp/{arch}/{device}.policy`は
  crosvm自身のRust製プロセス向けで、別バイナリの実QEMUには流用できない。
  Cuttlefishも実QEMUではなくcrosvmベースだった。nsjailは完全に汎用ツールで
  QEMU向け実績は見当たらず。libvirtもQEMU自身の内蔵`-sandbox`/`-seccomp`を
  有効にしているだけで、外部jailer側のポリシーではない）。ただしminijail自身が
  straceベースのポリシー生成ツール（`tools/generate_seccomp_policy.py`）を
  同梱しており、ゼロから自作するよりはこれを使ってstrace結果からポリシーを
  組み立てる方が楽——予定していた「strace等でのトレースに基づく作成」自体は
  変わらないが、そのための道具は既製のものを使える
- network namespace分離（tapのmove-in同期問題）は将来必要になれば着手する
