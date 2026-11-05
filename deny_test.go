package main

import (
	"os"
	"reflect"
	"testing"
)

func TestDenyExample(t *testing.T) {
	rules, err := loadDeny("deny.example")
	if err != nil {
		t.Fatal(err)
	}
	refused := []string{
		"disk/prep -w /dev/sdE0/plan9",
		"disk/fdisk -baw /dev/sdE0/data",
		"disk/format -b pbs /dev/sdE0/9fat",
		"hjfs -r -f /dev/sdE0/fs",
		"cwfs64x -c -f /dev/sdE0/fscache",
		"gefs -r -f /dev/sdE0/fs",
		"echo hi > /dev/sdE0/data",
		"cat x >>/dev/sdC0/ctl",
		"dd -if /dev/zero -of /dev/sdE0/data",
		"bind -a '#S' /dev",
		"fshalt -r",
		"echo reboot >/dev/reboot",
		"rm -rf /",
		"rm -r /*",
		"ls\nrm -rf / ",
	}
	allowed := []string{
		"ls -l /dev/sdE0",
		"cat /dev/sdE0/ctl",
		"grep -n prepare *.c",
		"mk install",
		"rm -rf /tmp/x",
		"rm -r ./build",
		"walk -f /sys/src | grep mkfs.c",
		"dd -if /dev/sdE0/data -of /tmp/mbr -count 1",
	}
	for _, c := range refused {
		if denied(rules, c, false) == nil {
			t.Errorf("not refused: %q", c)
		}
	}
	for _, c := range allowed {
		if r := denied(rules, c, false); r != nil {
			t.Errorf("refused: %q (rule %d)", c, r.line)
		}
	}
	for p, want := range map[string]bool{"/dev/sdE0/data": true, "#S/sdE0/ctl": true, "/adm/users": true,
		"/adm/timezone/local": false, "/tmp/sd": false} {
		if got := denied(rules, p, true) != nil; got != want {
			t.Errorf("path %s refused %v, want %v", p, got, want)
		}
	}
	if denied(rules, "/dev/sdE0/data", false) != nil && denied(rules, "x", true) != nil {
		t.Error("command and path rules mixed")
	}
}

func TestDenyCalls(t *testing.T) {
	c, dir := newTestServer(t)
	c.cfg.Deny = mustDeny(t, "\\brm\\b\npath:secret")
	c.cfg.DiskPaths = []string{dir + "/dev/sdE0"}
	c.init()
	c.fails("rc", map[string]any{"command": "echo a; rm x"}, "deny list (rule 1")
	c.ok("rc", map[string]any{"command": "echo farm"})
	c.fails("write", map[string]any{"path": "my.secret", "content": "x"}, "deny list (rule 2")
	c.fails("write", map[string]any{"path": "dev/sdE0/data", "content": "x"}, "disk device")
	c.fails("edit", map[string]any{"path": "dev/sdE0/ctl", "old_string": "a", "new_string": "b"}, "disk device")
	c.ok("write", map[string]any{"path": "dev/sdE00", "content": "x"})
}

func TestTokenize(t *testing.T) {
	for in, want := range map[string][]string{
		"bind -a '#S' /dev":          {"bind", "-a", "#S", "/dev"},
		"bind #S /dev":               {"bind", "#S", "/dev"},
		"mount -a '#s/x' /n/a 'b c'": {"mount", "-a", "#s/x", "/n/a", "b c"},
		"bind 'it''s' ''":            {"bind", "it's", ""},
		"  bind\t-b  /a\t/b ":        {"bind", "-b", "/a", "/b"},
	} {
		if got := tokenize(in); !reflect.DeepEqual(got, want) {
			t.Errorf("tokenize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := rcQuote("it's"); got != "'it''s'" {
		t.Errorf("rcQuote: %s", got)
	}
	if got := unquote("'rm 269: x ''/y'''"); got != "rm 269: x '/y'" {
		t.Errorf("unquote: %s", got)
	}
}

func mustDeny(t *testing.T, rules string) []denyRule {
	f := t.TempDir() + "/deny"
	if err := os.WriteFile(f, []byte(rules), 0644); err != nil {
		t.Fatal(err)
	}
	r, err := loadDeny(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
