package config

import "testing"

func TestHolderFromHostname(t *testing.T) {
	cases := map[string]string{
		"Mac-Mini.local":     "mac-mini",
		"build_box":          "build-box",
		"host01.example.com": "host01",
		"--":                 "",
	}
	for in, want := range cases {
		if got := HolderFromHostname(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"HISHAKU_URL": "https://manager.example.com/"}
	get := func(k string) string { return env[k] }
	host := func() (string, error) { return "Box.example.com", nil }
	c, err := FromEnv(get, host)
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://manager.example.com" || c.Holder != "box" {
		t.Fatalf("%+v", c)
	}

	env["HISHAKU_HOLDER"] = "gpu-box"
	if c, _ := FromEnv(get, host); c.Holder != "gpu-box" {
		t.Fatalf("holder %q", c.Holder)
	}

	env["HISHAKU_ACCESS_CLIENT_ID"] = "id-only"
	if _, err := FromEnv(get, host); err == nil {
		t.Fatal("id without secret should fail")
	}

	if _, err := FromEnv(func(string) string { return "" }, host); err == nil {
		t.Fatal("missing URL should fail")
	}
}
