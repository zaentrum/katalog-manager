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
