package gotunnel

import "testing"

func TestParseUpstreams(t *testing.T) {
	// Single bare base = one default route.
	r, err := parseUpstreams("http://127.0.0.1:43110")
	if err != nil || len(r) != 1 || r[0].prefix != "/" || r[0].base != "http://127.0.0.1:43110" {
		t.Fatalf("single = %+v %v", r, err)
	}
	// Mixed prefixed + default.
	r, err = parseUpstreams("/codex=http://127.0.0.1:43110, /api=http://127.0.0.1:8080, http://127.0.0.1:9000")
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 3 {
		t.Fatalf("want 3 routes, got %+v", r)
	}
	if r[0].prefix == "/" {
		t.Errorf("longest prefix must sort first: %+v", r)
	}
	if _, err := parseUpstreams(""); err == nil {
		t.Errorf("empty must error")
	}
	if _, err := parseUpstreams("noslash=http://127.0.0.1:1"); err == nil {
		t.Errorf("prefix without leading slash must error")
	}
}

func TestRouteFor(t *testing.T) {
	routes, err := parseUpstreams("/api/v1=http://127.0.0.1:1,/api=http://127.0.0.1:2,http://127.0.0.1:3")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"/api/v1/users": "http://127.0.0.1:1",
		"/api/v1":       "http://127.0.0.1:1",
		"/api/other":    "http://127.0.0.1:2",
		"/other":        "http://127.0.0.1:3",
		"/":             "http://127.0.0.1:3",
		// prefix boundary: /apix must NOT match /api
		"/apix": "http://127.0.0.1:3",
	}
	for path, want := range cases {
		if got := routeFor(routes, path); got.base != want {
			t.Errorf("route %q = %q, want %q", path, got.base, want)
		}
	}
}

func TestClientNames(t *testing.T) {
	got := clientNames(ClientOptions{Name: "A, b ,,C"})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("names = %q", got)
	}
	if got := clientNames(ClientOptions{}); len(got) != 1 || got[0] != "colab1" {
		t.Errorf("default = %q", got)
	}
}
