package main

import "testing"

func TestDeriveCloudImage(t *testing.T) {
	cases := []struct {
		runner, cloud, want string
	}{
		// Full Artifact Registry ref: registry + tag preserved, name swapped.
		{"us-central1-docker.pkg.dev/proj/claw/kube-claw-runner:0.2.1", "gcloud",
			"us-central1-docker.pkg.dev/proj/claw/kube-claw-gcloud:0.2.1"},
		{"docker.io/bitwavecode/kube-claw-runner:0.2.1", "aws",
			"docker.io/bitwavecode/kube-claw-aws:0.2.1"},
		{"kube-claw-runner:dev", "azure", "kube-claw-azure:dev"},
		// No recognizable runner component → can't derive.
		{"some/other-image:tag", "gcloud", ""},
	}
	for _, c := range cases {
		if got := deriveCloudImage(c.runner, c.cloud); got != c.want {
			t.Errorf("deriveCloudImage(%q,%q) = %q, want %q", c.runner, c.cloud, got, c.want)
		}
	}
}

func TestParseImageOverrides(t *testing.T) {
	got := parseImageOverrides(" gcloud=reg/gc:1 , aws=reg/aws:1 ,, bad-no-eq ")
	if got["gcloud"] != "reg/gc:1" {
		t.Errorf("gcloud override = %q", got["gcloud"])
	}
	if got["aws"] != "reg/aws:1" {
		t.Errorf("aws override = %q", got["aws"])
	}
	if _, ok := got["bad-no-eq"]; ok {
		t.Error("entry without '=' should be ignored")
	}
	if len(parseImageOverrides("")) != 0 {
		t.Error("empty string should yield no overrides")
	}
}
