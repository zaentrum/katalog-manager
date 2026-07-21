package downloads

import "testing"

// TestDerivedID locks the download read-model id to Java's
// UUID.nameUUIDFromBytes("adapter:clientId") (MD5-based UUIDv3). A drift here
// silently breaks the Kafka projection's dedup against any existing rows.
// Reference values computed independently (MD5 + version 0x30 / variant 0x80).
func TestDerivedID(t *testing.T) {
	cases := map[[2]string]string{
		{"odownloader", "job1"}:    "5f472259-4422-35cc-bfbe-cbc74bad1ea0",
		{"qbittorrent", "abcdef"}:  "5235399b-2d07-359b-8039-083fe71c6476",
		{"nzbget", "42"}:           "3af7137d-4367-3245-ad2f-5846e816a3b0",
	}
	for in, want := range cases {
		if got := derivedID(in[0], in[1]); got != want {
			t.Errorf("derivedID(%q,%q) = %s, want %s", in[0], in[1], got, want)
		}
	}
}

// TestDownloadTopicsFor locks the per-tenant prefix derivation: blank/"stube."
// keeps prod byte-identical; a tenant prefix (zaentrum-beta.) shifts all four.
func TestDownloadTopicsFor(t *testing.T) {
	prodDefault := []string{
		"stube.download.client.started",
		"stube.download.client.progress",
		"stube.download.client.completed",
		"stube.download.client.failed",
	}
	for _, prefix := range []string{"", "stube."} {
		got := downloadTopicsFor(prefix)
		for i, w := range prodDefault {
			if got[i] != w {
				t.Errorf("downloadTopicsFor(%q)[%d] = %s, want %s", prefix, i, got[i], w)
			}
		}
	}
	got := downloadTopicsFor("zaentrum-beta.")
	if got[2] != "zaentrum-beta.download.client.completed" {
		t.Errorf("tenant prefix: got %s", got[2])
	}
	if len(got) != 4 {
		t.Fatalf("want 4 topics, got %d", len(got))
	}
}
