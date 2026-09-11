package authz

import "strings"

// rpcService/rpcAction classify a gRPC full method name (e.g.
// "/kyuusha.blockstorage.v1.StorageConnectionService/Create") into the two
// facts policy.rego needs beyond claims/request.tenant_id, for the
// storage-admin and viewer roles (docs/specs/authn-authz.md "将来の拡張").
// Both are derived structurally -- from the proto package name every
// service's methods already live under, and the Get/List/Watch naming
// convention every read RPC in this codebase already follows -- rather
// than a hand-maintained per-method table, so adding a new service or RPC
// never requires touching this file.

// rpcService returns the proto package's second segment ("blockstorage",
// "compute", "identity", "image", "network"): fullMethod is always
// "/kyuusha.<service>.v1.<Service>/<Method>". Returns "" if fullMethod
// doesn't have that shape (defensive only -- every real gRPC call does).
func rpcService(fullMethod string) string {
	segs := strings.Split(fullMethod, "/")
	if len(segs) != 3 {
		return ""
	}
	pkgSegs := strings.Split(segs[1], ".")
	if len(pkgSegs) < 2 {
		return ""
	}
	return pkgSegs[1]
}

// rpcAction returns "read" for Get/List/Watch RPCs, "write" for everything
// else (Create/Update/Delete/Set*/Register/StreamConsole/...) -- a
// deliberately strict default: an RPC not recognized as read-only is never
// accidentally treated as safe for a read-only viewer.
func rpcAction(fullMethod string) string {
	segs := strings.Split(fullMethod, "/")
	if len(segs) != 3 {
		return "write"
	}
	method := segs[2]
	switch {
	case strings.HasPrefix(method, "Get"), strings.HasPrefix(method, "List"), strings.HasPrefix(method, "Watch"):
		return "read"
	default:
		return "write"
	}
}
