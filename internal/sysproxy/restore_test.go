package sysproxy

import "testing"

func TestIsOurs(t *testing.T) {
	for _, s := range []string{"socks=127.0.0.1:1080", "127.0.0.1:1080", "http=127.0.0.1:8080"} {
		if !isOurs(s) {
			t.Errorf("isOurs(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "socks=proxy.corp.local:1080", "10.0.0.1:3128"} {
		if isOurs(s) {
			t.Errorf("isOurs(%q) = true, want false", s)
		}
	}
}

func TestDecideRestore(t *testing.T) {
	cases := []struct {
		name        string
		haveSaved   bool
		savedEnable uint32
		savedServer string
		curServer   string
		wantEnable  uint32
		wantServer  string
	}{
		{
			name:      "no saved, our proxy live -> OFF (the reported bug)",
			haveSaved: false, curServer: "socks=127.0.0.1:1080",
			wantEnable: 0, wantServer: "",
		},
		{
			name:      "saved was ours (crash re-enable) -> OFF, not re-point",
			haveSaved: true, savedEnable: 1, savedServer: "socks=127.0.0.1:1080",
			curServer:  "socks=127.0.0.1:1080",
			wantEnable: 0, wantServer: "",
		},
		{
			name:      "user had a real corp proxy -> restore it",
			haveSaved: true, savedEnable: 1, savedServer: "socks=proxy.corp:1080",
			curServer:  "socks=127.0.0.1:1080",
			wantEnable: 1, wantServer: "socks=proxy.corp:1080",
		},
		{
			name:      "user had no proxy -> stays OFF",
			haveSaved: true, savedEnable: 0, savedServer: "",
			curServer:  "socks=127.0.0.1:1080",
			wantEnable: 0, wantServer: "",
		},
		{
			name:      "no saved, user set a non-ours proxy out-of-band -> keep it, don't force on",
			haveSaved: false, curServer: "socks=proxy.corp:1080",
			wantEnable: 0, wantServer: "socks=proxy.corp:1080",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotE, gotS, _ := decideRestore(c.haveSaved, c.savedEnable, c.savedServer, "", c.curServer)
			if gotE != c.wantEnable || gotS != c.wantServer {
				t.Errorf("decideRestore => (%d,%q), want (%d,%q)", gotE, gotS, c.wantEnable, c.wantServer)
			}
		})
	}
}
