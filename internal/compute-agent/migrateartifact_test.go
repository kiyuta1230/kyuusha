package computeagent

import "testing"

func TestParseMigrationArtifactURL(t *testing.T) {
	cases := []struct {
		url           string
		wantRef       string
		wantPlainHTTP bool
		wantErr       bool
	}{
		{url: "oci://registry.example.com/kyuusha-migration/vm-1:migrate-vm-1-123", wantRef: "registry.example.com/kyuusha-migration/vm-1:migrate-vm-1-123"},
		{url: "oci+http://localhost:5000/kyuusha-migration/vm-1:migrate-vm-1-123", wantRef: "localhost:5000/kyuusha-migration/vm-1:migrate-vm-1-123", wantPlainHTTP: true},
		{url: "https://not-an-oci-scheme/foo", wantErr: true},
		{url: "", wantErr: true},
	}
	for _, c := range cases {
		ref, plainHTTP, err := parseMigrationArtifactURL(c.url)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseMigrationArtifactURL(%q): expected error, got nil", c.url)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMigrationArtifactURL(%q): unexpected error: %v", c.url, err)
			continue
		}
		if ref != c.wantRef || plainHTTP != c.wantPlainHTTP {
			t.Errorf("parseMigrationArtifactURL(%q) = (%q, %v), want (%q, %v)", c.url, ref, plainHTTP, c.wantRef, c.wantPlainHTTP)
		}
	}
}
