package storage

import "testing"

func TestJoin(t *testing.T) {
	if got := Join("/bracer/", "", "shop", "main/", "shop", "x.dump"); got != "bracer/shop/main/shop/x.dump" {
		t.Errorf("Join = %q", got)
	}
}

func TestNewS3RejectsBadEndpoint(t *testing.T) {
	for _, ep := range []string{"", "s3.example.com", "ftp://x", "https://"} {
		if _, err := NewS3(S3Config{Endpoint: ep, Bucket: "b", AccessKey: "a", SecretKey: "s"}); err == nil {
			t.Errorf("endpoint %q must be rejected", ep)
		}
	}
	s, err := NewS3(S3Config{Endpoint: "https://acc.r2.cloudflarestorage.com", Bucket: "backups", AccessKey: "a", SecretKey: "s", PathStyle: true})
	if err != nil || s.Describe() != "s3 https://acc.r2.cloudflarestorage.com bucket backups" {
		t.Errorf("%v %v", s, err)
	}
}
