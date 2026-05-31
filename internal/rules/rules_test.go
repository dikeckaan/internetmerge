package rules

import "testing"

func TestResolveHostRules(t *testing.T) {
	s := New()
	s.Replace([]Rule{
		{HostGlob: "*.example.com", Action: Direct},
		{Port: 443, Action: Link, IfName: "en0"},
		{HostGlob: "blocked.test", Action: Block},
	}, nil)

	cases := []struct {
		host   string
		port   uint16
		action string
		ifName string
	}{
		{"a.example.com", 80, Direct, ""},
		{"example.com", 80, Direct, ""}, // *.example.com matches bare domain
		{"other.net", 443, Link, "en0"}, // port rule
		{"other.net", 80, Bond, ""},     // no match -> bond
		{"blocked.test", 1234, Block, ""},
	}
	for _, c := range cases {
		d := s.Resolve(c.host, c.port, "")
		if d.Action != c.action || d.IfName != c.ifName {
			t.Errorf("Resolve(%s:%d) = %+v, want {%s %s}", c.host, c.port, d, c.action, c.ifName)
		}
	}
}

func TestFirstMatchWins(t *testing.T) {
	s := New()
	s.Replace([]Rule{
		{HostGlob: "*.example.com", Action: Direct},
		{HostGlob: "*.example.com", Action: Block}, // shadowed by the first
	}, nil)
	if d := s.Resolve("x.example.com", 80, ""); d.Action != Direct {
		t.Fatalf("first match should win, got %s", d.Action)
	}
}

func TestAppRulesTakePrecedence(t *testing.T) {
	s := New()
	s.Replace(
		[]Rule{{HostGlob: "*", Action: Bond}},
		[]AppRule{{Exe: "chrome.exe", Action: Direct}},
	)
	// exe match -> Direct, even though host rule says Bond.
	if d := s.Resolve("anything.com", 80, `C:\Program Files\Google\Chrome\chrome.exe`); d.Action != Direct {
		t.Fatalf("app rule should win, got %s", d.Action)
	}
	// unknown exe -> falls through to host rule.
	if d := s.Resolve("anything.com", 80, `C:\x\other.exe`); d.Action != Bond {
		t.Fatalf("non-matching exe should bond, got %s", d.Action)
	}
}

func TestDefaultIsBond(t *testing.T) {
	s := New()
	if d := s.Resolve("x.com", 80, ""); d.Action != Bond {
		t.Fatalf("empty ruleset should default to bond, got %s", d.Action)
	}
}
