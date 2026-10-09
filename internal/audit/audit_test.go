package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestLog_ServiceAccountFields(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	Log(context.Background(), Record{
		Event: EventRPCCompleted, RPCMethod: "/kyuusha.network.v1.SubnetService/Create", RequestTenantID: "tenant-b",
		TenantID: "ops", Sub: "8a1f0c2e-uuid", Roles: []string{"network-admin", "viewer"},
		ClientID: "kyuusha-vpc", Username: "service-account-kyuusha-vpc",
	})
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("parse %q: %v", buf.String(), err)
	}
	roles, _ := got["roles"].([]any)
	if got["audit"] != true || got["client_id"] != "kyuusha-vpc" || got["username"] != "service-account-kyuusha-vpc" ||
		got["sub"] != "8a1f0c2e-uuid" || len(roles) != 2 || roles[0] != "network-admin" {
		t.Fatalf("audit record = %v", got)
	}
}
