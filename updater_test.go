package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},
		{"v0.2.0", "0.10.0", -1},
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"2", "10.0.0", -1},
		{"v1.2.3-beta", "1.2.2", 1},
		{"1.0", "1.0.0", 0},
		{"garbage", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVersionParts(t *testing.T) {
	if got := versionParts("v1.2.3"); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("bad parse: %v", got)
	}
	if versionParts("nope") != nil {
		t.Fatal("expected nil for garbage")
	}
}

func TestAssetBaseName(t *testing.T) {
	name := assetBaseName()
	if name == "" || name[0] != 's' {
		t.Fatalf("bad asset name %q", name)
	}
}

func TestCheckForUpdate(t *testing.T) {
	old := ghAPIBase
	defer func() { ghAPIBase = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v9.9.9","html_url":"http://x/r",
			"assets":[{"name":"`+assetBaseName()+`","browser_download_url":"http://x/dl"},
			          {"name":"`+assetBaseName()+`.sha256","browser_download_url":"http://x/sum"}]}`)
	}))
	defer srv.Close()
	ghAPIBase = srv.URL

	info, err := checkForUpdate(context.Background(), "o/r", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Available || info.Latest != "v9.9.9" || info.DownloadURL != "http://x/dl" || info.ChecksumURL != "http://x/sum" {
		t.Fatalf("bad info: %+v", info)
	}

	// Same version → not available.
	info2, _ := checkForUpdate(context.Background(), "o/r", "9.9.9")
	if info2.Available {
		t.Fatal("should not be available for same version")
	}

	// Empty repo → unconfigured, no error.
	info3, err := checkForUpdate(context.Background(), "", "0.1.0")
	if err != nil || info3.Configured {
		t.Fatalf("empty repo: %+v err=%v", info3, err)
	}
}

func TestCheckForUpdate404(t *testing.T) {
	old := ghAPIBase
	defer func() { ghAPIBase = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	ghAPIBase = srv.URL
	info, err := checkForUpdate(context.Background(), "o/r", "0.1.0")
	if err == nil || info.Available {
		t.Fatalf("expected error, got %+v", info)
	}
}
